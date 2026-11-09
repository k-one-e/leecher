package main

// killswitch.go — stop every download when the outbound IP is not one the
// admin allowed, e.g. because the VPN dropped and traffic leaks out over the
// ISP line.
//
// While armed, a monitor looks up the public IP every few seconds, and every
// new download, probe or torrent start re-checks when the last answer is
// older than one interval. A wrong IP — or no answer at all — trips it:
//   - yt-dlp and direct downloads are stopped and their partial files removed
//   - built-in torrents are paused and the torrent engine is shut down
//     entirely (no peers, trackers or DHT); pieces on disk are kept
//   - running torrents in an external Transmission are stopped
//   - new downloads, link checks and thumbnails are refused
// Once an allowed IP is seen again (or the switch is turned off) paused
// torrents resume and Transmission torrents it stopped are started again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Plain-text, IPv4-only services: mixing in one that answers over IPv6 would
// make the reported address flip between families and trip the switch.
var killSwitchProviders = []string{
	"https://api.ipify.org",
	"https://checkip.amazonaws.com",
	"https://ipv4.icanhazip.com",
}

var killSwitchLookupTimeout = 4 * time.Second // per service; a var so tests can shorten it

var errKillSwitch = errors.New("kill switch")

type killSwitchState struct {
	mu        sync.Mutex
	armed     bool // enabled the last time the monitor ran
	verified  bool // the last check saw an allowed IP
	tripped   bool // a check failed or saw a foreign IP; downloads are stopped
	ip        string
	reason    string
	checkedAt time.Time
	trippedAt time.Time
	provider  int
	stopCtx   context.Context // cancelled when the switch trips
	stopFn    context.CancelFunc
}

var (
	ks          = &killSwitchState{}
	ksCheckMu   sync.Mutex // one lookup at a time; concurrent callers reuse its result
	ksKick      = make(chan struct{}, 1)
	ksTrMu      sync.Mutex
	ksTrStopped []string // Transmission hashes stopped by the switch
)

func init() { ks.stopCtx, ks.stopFn = context.WithCancel(context.Background()) }

func killSwitchInterval() time.Duration {
	s := cfg().KillSwitchIntervalSecs
	if s < 1 {
		s = 3
	}
	return time.Duration(s) * time.Second
}

// parseAllowedIPs accepts single addresses and CIDR ranges.
func parseAllowedIPs(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid IP range", e)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid IP address", e)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// normalizeAllowedIPs validates and de-duplicates the admin's list.
func normalizeAllowedIPs(entries []string) ([]string, error) {
	prefixes, err := parseAllowedIPs(entries)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range prefixes {
		s := p.String()
		if p.IsSingleIP() {
			s = p.Addr().String()
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func ipAllowed(ip string, allowed []netip.Prefix) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range allowed {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// lookupOutboundIP asks the IP services in turn, starting with a different one
// each time to spread the load. It reuses egressClient, which never keeps a
// connection alive: a pooled connection keeps the route it was opened on.
func lookupOutboundIP(ctx context.Context) (string, error) {
	ks.mu.Lock()
	start := ks.provider
	ks.provider = (ks.provider + 1) % len(killSwitchProviders)
	ks.mu.Unlock()
	var errs []string
	for i := range killSwitchProviders {
		url := killSwitchProviders[(start+i)%len(killSwitchProviders)]
		ip, err := fetchPlainIP(ctx, url)
		if err == nil {
			return ip, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		errs = append(errs, err.Error())
	}
	return "", errors.New(strings.Join(errs, "; "))
}

func fetchPlainIP(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, killSwitchLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", directUserAgent)
	resp, err := egressClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %s", url, friendlyNetError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", fmt.Errorf("%s: %v", url, err)
	}
	a, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return "", fmt.Errorf("%s: unexpected answer", url)
	}
	return a.Unmap().String(), nil
}

// killSwitchCheck runs one lookup and applies the result. A caller that had to
// wait for a lookup already in flight takes that result instead of starting
// another one. The lookup always runs to completion on its own budget, so a
// hanging service counts as a failed lookup (and trips), never as "no answer".
func killSwitchCheck(ctx context.Context) {
	asked := time.Now()
	ksCheckMu.Lock()
	defer ksCheckMu.Unlock()
	ks.mu.Lock()
	recent := ks.checkedAt.After(asked)
	ks.mu.Unlock()
	if recent {
		return
	}

	if !cfg().KillSwitch {
		killSwitchDisarmLocked()
		return
	}
	ks.mu.Lock()
	ks.armed = true
	ks.mu.Unlock()

	budget := time.Duration(len(killSwitchProviders))*killSwitchLookupTimeout + time.Second
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	ip, err := lookupOutboundIP(lctx)
	cancel()

	// The admin may have changed the settings while the lookup ran.
	c := cfg()
	if !c.KillSwitch {
		killSwitchDisarmLocked()
		return
	}
	allowed, _ := parseAllowedIPs(c.KillSwitchIPs)
	switch {
	case err != nil:
		killSwitchTrip("", "the outbound IP could not be verified")
		logger.Warn("Kill switch lookup failed", "error", err)
	case !ipAllowed(ip, allowed):
		killSwitchTrip(ip, fmt.Sprintf("the outbound IP changed to %s", ip))
	default:
		killSwitchRecover(ip)
	}
}

func killSwitchTrip(ip, reason string) {
	ks.mu.Lock()
	first := !ks.tripped
	ks.tripped, ks.verified = true, false
	ks.ip, ks.reason, ks.checkedAt = ip, reason, time.Now()
	if first {
		ks.trippedAt = ks.checkedAt
		ks.stopFn()
	}
	ks.mu.Unlock()
	if ip != "" {
		go invalidateEgress(ip)
	}
	if first {
		logger.Warn("Kill switch tripped: stopping all downloads", "reason", reason)
	}
	// Every check while tripped sweeps again, in case a download slipped in
	// between passing its gate and the trip, or stopping Transmission failed.
	stopDownloadsForKillSwitch("Stopped by the kill switch: " + reason + ".")
	stopTransmissionForKillSwitch()
}

func killSwitchRecover(ip string) {
	ks.mu.Lock()
	wasBlocked := !ks.verified
	wasTripped := ks.tripped
	ks.tripped, ks.verified = false, true
	ks.ip, ks.reason, ks.checkedAt = ip, "", time.Now()
	if wasTripped {
		ks.stopCtx, ks.stopFn = context.WithCancel(context.Background())
	}
	ks.mu.Unlock()
	go invalidateEgress(ip)
	if wasTripped {
		logger.Info("Kill switch cleared: outbound IP allowed again", "ip", ip)
	}
	if wasBlocked {
		resumeTorrents()
		startTransmissionAfterKillSwitch()
	}
}

// killSwitchDisarmLocked forgets all state when the switch is turned off and
// lets anything it paused carry on. Callers hold ksCheckMu, so it cannot
// interleave with a check that is about to trip.
func killSwitchDisarmLocked() {
	ks.mu.Lock()
	wasArmed, wasTripped, wasVerified := ks.armed, ks.tripped, ks.verified
	ks.armed, ks.tripped, ks.verified = false, false, false
	ks.ip, ks.reason = "", ""
	if wasTripped {
		ks.stopCtx, ks.stopFn = context.WithCancel(context.Background())
	}
	ks.mu.Unlock()
	// Not verified also covers torrents held back at startup before the first check.
	if wasArmed && !wasVerified {
		logger.Info("Kill switch turned off while tripped; resuming downloads")
		resumeTorrents()
		startTransmissionAfterKillSwitch()
	}
}

// killSwitchBlocked reports, without a lookup, whether downloads are currently
// held back: armed and not (yet) confirmed on an allowed IP.
func killSwitchBlocked() (bool, string) {
	if !cfg().KillSwitch {
		return false, ""
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.verified {
		return false, ""
	}
	if ks.reason == "" {
		return true, "the outbound IP has not been verified yet"
	}
	return true, ks.reason
}

// killSwitchGate is called before anything makes an outbound request on a
// user's behalf. It re-checks when the last answer is older than one
// interval, and returns a user-facing error while downloads are blocked.
func killSwitchGate(ctx context.Context) error {
	if !cfg().KillSwitch {
		return nil
	}
	ks.mu.Lock()
	stale := time.Since(ks.checkedAt) >= killSwitchInterval()
	ks.mu.Unlock()
	if stale {
		killSwitchCheck(ctx)
	}
	if blocked, reason := killSwitchBlocked(); blocked {
		return killSwitchErr{reason}
	}
	return nil
}

// killSwitchErr is the user-facing refusal; errors.Is(err, errKillSwitch) matches it.
type killSwitchErr struct{ reason string }

func (e killSwitchErr) Error() string {
	return "Downloads are paused by the kill switch — " + e.reason + "."
}
func (killSwitchErr) Is(target error) bool { return target == errKillSwitch }

// withKillSwitch derives a context that is also cancelled when the switch
// trips, for short outbound work such as link checks.
func withKillSwitch(parent context.Context) (context.Context, context.CancelFunc) {
	ks.mu.Lock()
	stop := ks.stopCtx
	ks.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	unregister := context.AfterFunc(stop, cancel)
	return ctx, func() { unregister(); cancel() }
}

func kickKillSwitch() {
	select {
	case ksKick <- struct{}{}:
	default:
	}
}

// ksResetChecked makes the next gate look the IP up again instead of trusting
// a result obtained under the previous settings.
func ksResetChecked() {
	ks.mu.Lock()
	ks.checkedAt = time.Time{}
	ks.mu.Unlock()
}

func killSwitchLoop() {
	for {
		if cfg().KillSwitch {
			killSwitchCheck(context.Background())
		} else {
			ksCheckMu.Lock()
			killSwitchDisarmLocked()
			ksCheckMu.Unlock()
		}
		select {
		case <-time.After(killSwitchInterval()):
		case <-ksKick:
		case <-shutdownCh:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Stopping and resuming
// ---------------------------------------------------------------------------

func stopDownloadsForKillSwitch(reason string) {
	jobsMu.RLock()
	live := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		live = append(live, j)
	}
	jobsMu.RUnlock()
	for _, j := range live {
		if j.Kind == kindTorrent {
			continue
		}
		j.mu.Lock()
		active := j.Status == "queued" || j.Status == "downloading"
		if active {
			j.Status, j.Message, j.StopReason = "cancelled", reason, reason
			j.notifySubscribers()
			j.cancel()
		}
		j.mu.Unlock()
		if active {
			os.RemoveAll(filepath.Join(jobWorkDir(j), j.ID))
			logger.Warn("Kill switch stopped download", "job", j.ID)
		}
	}
	pauseTorrentsForKillSwitch(reason)
}

// pauseTorrentsForKillSwitch pauses every built-in torrent and closes the
// engine, which drops all peer connections, tracker announces and the DHT.
// The resume list and the pieces on disk are kept for resumeTorrents.
func pauseTorrentsForKillSwitch(reason string) {
	btMu.Lock()
	running := make([]*btJob, 0, len(btJobs))
	for _, b := range btJobs {
		running = append(running, b)
	}
	cl := btClient
	btClient = nil
	btMu.Unlock()

	for _, b := range running {
		job := b.job
		job.mu.Lock()
		switch {
		case job.Status == "queued" || job.Status == "downloading":
			b.paused.Store(true)
			job.Status, job.Message, job.Peers = "paused", strings.Replace(reason, "Stopped", "Paused", 1), 0
			job.notifySubscribers()
			job.cancel()
		case job.Seeding:
			b.stopSeeding()
		}
		job.mu.Unlock()
	}
	if cl != nil {
		cl.Close()
		logger.Warn("Kill switch shut down the torrent engine", "torrents", len(running))
	}
}

func killSwitchStatePath() string { return filepath.Join(stateDir(), "killswitch.json") }

// The list of stopped Transmission torrents survives a restart, so they are
// still started again once the IP is back.
func saveTransmissionStoppedLocked() {
	data, _ := json.Marshal(ksTrStopped)
	os.MkdirAll(stateDir(), 0700)
	tmp := killSwitchStatePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		os.Rename(tmp, killSwitchStatePath())
	}
}

func loadTransmissionStopped() {
	if data, err := os.ReadFile(killSwitchStatePath()); err == nil {
		json.Unmarshal(data, &ksTrStopped)
	}
}

func stopTransmissionForKillSwitch() {
	if !transmissionEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := getTorrents(ctx, nil)
	if err != nil {
		logger.Warn("Kill switch could not list Transmission torrents", "error", err)
		return
	}
	var hashes []string
	for _, t := range list {
		if t.Status != 0 { // 0 = stopped
			hashes = append(hashes, t.HashString)
		}
	}
	if len(hashes) == 0 {
		return
	}
	if err := trClient.call(ctx, "torrent-stop", map[string]any{"ids": hashes}, nil); err != nil {
		logger.Warn("Kill switch could not stop Transmission torrents", "error", err)
		return
	}
	ksTrMu.Lock()
	seen := map[string]bool{}
	for _, h := range ksTrStopped {
		seen[h] = true
	}
	for _, h := range hashes {
		if !seen[h] {
			ksTrStopped = append(ksTrStopped, h)
		}
	}
	saveTransmissionStoppedLocked()
	ksTrMu.Unlock()
	logger.Warn("Kill switch stopped Transmission torrents", "count", len(hashes))
}

func startTransmissionAfterKillSwitch() {
	ksTrMu.Lock()
	hashes := append([]string(nil), ksTrStopped...)
	ksTrMu.Unlock()
	if len(hashes) == 0 || !transmissionEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := trClient.call(ctx, "torrent-start", map[string]any{"ids": hashes}, nil); err != nil {
		logger.Warn("Could not restart Transmission torrents after the kill switch", "error", err)
		return
	}
	ksTrMu.Lock()
	ksTrStopped = nil
	saveTransmissionStoppedLocked()
	ksTrMu.Unlock()
	logger.Info("Restarted Transmission torrents stopped by the kill switch", "count", len(hashes))
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

type killSwitchStatus struct {
	Enabled   bool      `json:"enabled"`
	Tripped   bool      `json:"tripped"`
	Verified  bool      `json:"verified"`
	IP        string    `json:"ip,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	TrippedAt time.Time `json:"tripped_at,omitempty"`
}

func currentKillSwitchStatus() killSwitchStatus {
	if !cfg().KillSwitch {
		return killSwitchStatus{}
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	st := killSwitchStatus{Enabled: true, Tripped: ks.tripped, Verified: ks.verified, IP: ks.ip, Reason: ks.reason, CheckedAt: ks.checkedAt}
	if ks.tripped {
		st.TrippedAt = ks.trippedAt
	}
	return st
}

// handleAdminKillSwitch: GET returns the live state; GET ?detect=1 looks up
// the current outbound IP so the admin can add it to the allowed list.
func handleAdminKillSwitch(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Query().Get("detect") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(len(killSwitchProviders))*killSwitchLookupTimeout+time.Second)
		defer cancel()
		ip, err := lookupOutboundIP(ctx)
		if err != nil {
			jsonError(w, "Could not determine the outbound IP: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ip": ip})
		return
	}
	writeJSON(w, http.StatusOK, currentKillSwitchStatus())
}
