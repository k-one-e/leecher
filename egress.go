package main

// egress.go — which public IP (and where) leecher's downloads come from.
//
// The lookup uses the same dialer and SSRF guard as direct downloads, so it
// reflects the real egress path (e.g. a VPN gateway) rather than the address
// the UI is reached on. Keep-alives are off: a pooled connection opened before
// a VPN switch keeps its old route and would report a stale address forever.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	egressTTL        = 10 * time.Minute
	egressMinRefresh = 30 * time.Second
)

type egressInfo struct {
	IP        string    `json:"ip"`
	City      string    `json:"city,omitempty"`
	Region    string    `json:"region,omitempty"`
	Country   string    `json:"country,omitempty"` // ISO 3166-1 alpha-2
	Org       string    `json:"org,omitempty"`
	Source    string    `json:"source,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`

	KillSwitch *killSwitchStatus `json:"kill_switch,omitempty"` // added per response, never cached
}

type egressProvider struct {
	name  string
	url   string
	parse func([]byte) (egressInfo, error)
}

// Tried in order; both are keyless and return the caller's own address.
var egressProviders = []egressProvider{
	{"ipinfo.io", "https://ipinfo.io/json", func(b []byte) (egressInfo, error) {
		var r struct{ IP, City, Region, Country, Org string }
		if err := json.Unmarshal(b, &r); err != nil {
			return egressInfo{}, err
		}
		return egressInfo{IP: r.IP, City: r.City, Region: r.Region, Country: r.Country, Org: r.Org}, nil
	}},
	{"ipwho.is", "https://ipwho.is/", func(b []byte) (egressInfo, error) {
		var r struct {
			Success     bool   `json:"success"`
			Message     string `json:"message"`
			IP          string `json:"ip"`
			City        string `json:"city"`
			Region      string `json:"region"`
			CountryCode string `json:"country_code"`
			Connection  struct {
				Org string `json:"org"`
				ISP string `json:"isp"`
			} `json:"connection"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return egressInfo{}, err
		}
		if !r.Success {
			return egressInfo{}, errors.New(r.Message)
		}
		return egressInfo{IP: r.IP, City: r.City, Region: r.Region, Country: r.CountryCode, Org: firstNonEmpty(r.Connection.Org, r.Connection.ISP)}, nil
	}},
}

var egressClient = func() *http.Client {
	c := newDirectClient()
	c.Transport.(*http.Transport).DisableKeepAlives = true
	return c
}()

var (
	egressMu    sync.Mutex
	egressCache *egressInfo
)

func lookupEgress(ctx context.Context) (egressInfo, error) {
	var errs []string
	for _, p := range egressProviders {
		info, err := fetchEgress(ctx, p)
		if err == nil && info.IP != "" {
			info.Source = p.name
			info.Country = strings.ToUpper(info.Country)
			return info, nil
		}
		if err == nil {
			err = errors.New("no IP in response")
		}
		errs = append(errs, p.name+": "+err.Error())
	}
	return egressInfo{}, errors.New(strings.Join(errs, "; "))
}

func fetchEgress(ctx context.Context, p egressProvider) (egressInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return egressInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", directUserAgent)
	resp, err := egressClient.Do(req)
	if err != nil {
		return egressInfo{}, errors.New(friendlyNetError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return egressInfo{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return egressInfo{}, err
	}
	return p.parse(body)
}

// currentEgress returns the cached result, refreshing it when stale or when
// force is set (throttled so the UI cannot hammer the lookup services).
func currentEgress(ctx context.Context, force bool) egressInfo {
	egressMu.Lock()
	defer egressMu.Unlock()
	if c := egressCache; c != nil {
		age := time.Since(c.CheckedAt)
		if age < egressTTL && (!force || age < egressMinRefresh) {
			return *c
		}
	}
	info, err := lookupEgress(ctx)
	info.CheckedAt = time.Now()
	if err != nil {
		logger.Warn("Egress lookup failed", "error", err)
		info.Error = "Could not determine the public IP."
		// Keep showing the last good answer, marked as stale.
		if c := egressCache; c != nil && c.Error == "" {
			stale := *c
			stale.Error = info.Error
			return stale
		}
	} else if egressCache == nil || egressCache.IP != info.IP {
		logger.Info("Egress address", "ip", info.IP, "city", info.City, "country", info.Country, "source", info.Source)
	}
	egressCache = &info
	return info
}

func handleEgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info := currentEgress(ctx, r.URL.Query().Get("refresh") == "1")
	if st := currentKillSwitchStatus(); st.Enabled {
		info.KillSwitch = &st
	}
	writeJSON(w, http.StatusOK, info)
}

// invalidateEgress drops the cached lookup when the kill switch monitor sees
// a different address, so the badge follows a VPN change within one refresh.
func invalidateEgress(ip string) {
	egressMu.Lock()
	defer egressMu.Unlock()
	if egressCache != nil && egressCache.IP != ip {
		egressCache = nil
	}
}
