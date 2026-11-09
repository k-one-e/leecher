package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeTransmission emulates the bits of Transmission's RPC that leecher uses,
// including the 409 session-id handshake and basic auth.
type fakeTransmission struct {
	mu       sync.Mutex
	torrents map[string]string // hash -> name
	added    []map[string]any
	removed  []string
}

func (f *fakeTransmission) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "tr-user" || p != "tr-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Transmission-Session-Id") != "sid-1" {
			w.Header().Set("X-Transmission-Session-Id", "sid-1")
			w.WriteHeader(http.StatusConflict)
			return
		}
		var req struct {
			Method    string         `json:"method"`
			Arguments map[string]any `json:"arguments"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := map[string]any{}
		switch req.Method {
		case "torrent-add":
			f.added = append(f.added, req.Arguments)
			hash, name := "0123456789abcdef0123456789abcdef01234567", "ubuntu.iso"
			if _, ok := req.Arguments["metainfo"]; ok {
				hash, name = "fedcba9876543210fedcba9876543210fedcba98", "from-file"
			}
			key := "torrent-added"
			if _, dup := f.torrents[hash]; dup {
				key = "torrent-duplicate"
			}
			f.torrents[hash] = name
			reply[key] = map[string]any{"hashString": hash, "name": name, "id": 1}
		case "torrent-get":
			var list []map[string]any
			want := map[string]bool{}
			ids, filtered := req.Arguments["ids"].([]any)
			for _, id := range ids {
				want[id.(string)] = true
			}
			for h, n := range f.torrents {
				if filtered && !want[h] {
					continue
				}
				list = append(list, map[string]any{"hashString": h, "name": n, "status": 4, "percentDone": 0.5, "sizeWhenDone": 1000})
			}
			reply["torrents"] = list
		case "torrent-remove":
			for _, id := range req.Arguments["ids"].([]any) {
				delete(f.torrents, id.(string))
				f.removed = append(f.removed, id.(string))
			}
		case "session-get":
			reply["version"] = "4.0.6"
			reply["download-dir"] = "/downloads/complete"
		}
		json.NewEncoder(w).Encode(map[string]any{"result": "success", "arguments": reply})
	}
}

func setupTransmission(t *testing.T) *fakeTransmission {
	t.Helper()
	fake := &fakeTransmission{torrents: map[string]string{}}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	t.Setenv("LEECHER_HOME", t.TempDir())
	t.Setenv("TRANSMISSION_USER", "tr-user")
	t.Setenv("TRANSMISSION_PASS", "tr-pass")
	withConfig(t, func(c *Config) {
		c.TorrentEngine = engineTransmission
		c.TransmissionURL = srv.URL + "/transmission/rpc"
		c.TransmissionDownloadDir = "/downloads/leecher"
		c.DownloadsDir = t.TempDir()
	})
	torrentOwnersMu.Lock()
	torrentOwners = map[string]torrentOwner{}
	torrentOwnersMu.Unlock()
	trClient = &transmissionClient{hc: &http.Client{}}
	oldLimiter := limiter
	limiter = newRateLimiter()
	t.Cleanup(func() { limiter = oldLimiter })
	return fake
}

func asUser(t *testing.T, r *http.Request, name string) *http.Request {
	u, ok := userDB.get(name)
	if !ok {
		t.Fatalf("no user %s", name)
	}
	return withAuth(r, &authInfo{User: u})
}

func TestTransmission_MagnetFlowAndOwnership(t *testing.T) {
	withUsers(t)
	if err := userDB.create("carol", "carol-password", roleUser, false); err != nil {
		t.Fatal(err)
	}
	fake := setupTransmission(t)
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=ubuntu"

	req := httptest.NewRequest("POST", "/api/download", strings.NewReader(`{"url":"`+magnet+`"}`))
	w := httptest.NewRecorder()
	handleDownload(w, asUser(t, req, "bob"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"torrent"`) {
		t.Fatalf("magnet should be handed to Transmission, got %d %s", w.Code, w.Body)
	}
	if len(fake.added) != 1 || fake.added[0]["filename"] != magnet || fake.added[0]["download-dir"] != "/downloads/leecher" {
		t.Fatalf("unexpected torrent-add arguments: %+v", fake.added)
	}

	list := func(name string) []torrentView {
		w := httptest.NewRecorder()
		handleTorrents(w, asUser(t, httptest.NewRequest("GET", "/api/torrents", nil), name))
		var resp struct {
			Torrents []torrentView `json:"torrents"`
		}
		json.NewDecoder(w.Body).Decode(&resp)
		return resp.Torrents
	}
	if got := list("bob"); len(got) != 1 || got[0].Name != "ubuntu.iso" || got[0].Owner != "" {
		t.Errorf("bob should see his torrent without owner info, got %+v", got)
	}
	if got := list("carol"); len(got) != 0 {
		t.Errorf("carol must not see bob's torrent, got %+v", got)
	}
	if got := list("alice"); len(got) != 1 || got[0].Owner != "bob" {
		t.Errorf("admins see all torrents with their owner, got %+v", got)
	}

	// Carol re-adding the same magnet must not hand her bob's torrent.
	w = httptest.NewRecorder()
	handleDownload(w, asUser(t, httptest.NewRequest("POST", "/api/download", strings.NewReader(`{"url":"`+magnet+`"}`)), "carol"))
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate from another user should conflict, got %d", w.Code)
	}

	hash := "0123456789abcdef0123456789abcdef01234567"
	w = httptest.NewRecorder()
	handleTorrents(w, asUser(t, httptest.NewRequest("POST", "/api/torrents/"+hash+"/remove-data", nil), "carol"))
	if w.Code != http.StatusNotFound || len(fake.removed) != 0 {
		t.Errorf("carol must not remove bob's torrent, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	handleTorrents(w, asUser(t, httptest.NewRequest("POST", "/api/torrents/"+hash+"/remove", nil), "bob"))
	if w.Code != 200 || len(fake.removed) != 1 {
		t.Errorf("bob should remove his own torrent, got %d %s", w.Code, w.Body)
	}
	if got := list("bob"); len(got) != 0 {
		t.Errorf("removed torrent should be gone, got %+v", got)
	}
}

func TestTransmission_UploadTorrentFile(t *testing.T) {
	withUsers(t)
	fake := setupTransmission(t)
	body, ctype := multipartFile(t, "file", "x.torrent", []byte("d4:infod4:name1:xee"))
	req := httptest.NewRequest("POST", "/api/torrents/upload", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	handleTorrents(w, asUser(t, req, "bob"))
	if w.Code != 200 {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body)
	}
	if mi, _ := fake.added[0]["metainfo"].(string); mi != base64.StdEncoding.EncodeToString([]byte("d4:infod4:name1:xee")) {
		t.Errorf("torrent bytes should be sent as base64 metainfo, got %q", mi)
	}

	body, ctype = multipartFile(t, "file", "x.torrent", []byte("<html>not a torrent</html>"))
	req = httptest.NewRequest("POST", "/api/torrents/upload", body)
	req.Header.Set("Content-Type", ctype)
	w = httptest.NewRecorder()
	handleTorrents(w, asUser(t, req, "bob"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("garbage uploads should be rejected, got %d", w.Code)
	}
}

func TestTransmission_SettingsValidation(t *testing.T) {
	u, d := " http://transmission-service:9091 ", "/downloads//complete/"
	if err := validateTransmissionSettings(&u, &d); err != nil {
		t.Fatal(err)
	}
	if u != "http://transmission-service:9091/transmission/rpc" || d != "/downloads/complete" {
		t.Errorf("normalisation failed: %q %q", u, d)
	}
	u, d = "transmission:9091", ""
	if err := validateTransmissionSettings(&u, &d); err == nil {
		t.Error("URLs without scheme must be rejected")
	}
	u, d = "", "relative/dir"
	if err := validateTransmissionSettings(&u, &d); err == nil {
		t.Error("relative download dirs must be rejected")
	}
}

func TestTransmission_TestEndpoint(t *testing.T) {
	withUsers(t)
	setupTransmission(t)
	w := httptest.NewRecorder()
	handleAdminTransmissionTest(w, asUser(t, httptest.NewRequest("GET", "/api/admin/transmission-test", nil), "alice"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "4.0.6") {
		t.Errorf("expected version in response, got %d %s", w.Code, w.Body)
	}
}

func multipartFile(t *testing.T, field, name string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	return &buf, mw.FormDataContentType()
}
