package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withEgressProviders(t *testing.T, first, second http.HandlerFunc) (*atomic.Int32, *atomic.Int32) {
	t.Helper()
	var n1, n2 atomic.Int32
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n1.Add(1); first(w, r) }))
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n2.Add(1); second(w, r) }))
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	orig := egressProviders
	egressProviders = []egressProvider{
		{"ipinfo.io", s1.URL, orig[0].parse},
		{"ipwho.is", s2.URL, orig[1].parse},
	}
	withConfig(t, func(c *Config) { c.AllowPrivateURLs = true }) // httptest is on loopback
	egressMu.Lock()
	egressCache = nil
	egressMu.Unlock()
	t.Cleanup(func() {
		egressProviders = orig
		egressMu.Lock()
		egressCache = nil
		egressMu.Unlock()
	})
	return &n1, &n2
}

func jsonReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}

func TestEgress_UsesFirstProviderAndCaches(t *testing.T) {
	n1, n2 := withEgressProviders(t,
		jsonReply(`{"ip":"203.0.113.7","city":"Frankfurt am Main","region":"Hesse","country":"de","org":"AS3320 Deutsche Telekom AG"}`),
		jsonReply(`{"success":true,"ip":"198.51.100.1"}`))

	got := currentEgress(context.Background(), false)
	if got.IP != "203.0.113.7" || got.City != "Frankfurt am Main" || got.Country != "DE" || got.Source != "ipinfo.io" || got.Error != "" {
		t.Fatalf("unexpected result %+v", got)
	}
	currentEgress(context.Background(), false)
	currentEgress(context.Background(), true) // forced, but within the minimum refresh interval
	if n1.Load() != 1 || n2.Load() != 0 {
		t.Errorf("expected one lookup thanks to caching, got %d/%d", n1.Load(), n2.Load())
	}

	egressMu.Lock()
	egressCache.CheckedAt = time.Now().Add(-egressMinRefresh - time.Second)
	egressMu.Unlock()
	currentEgress(context.Background(), true)
	if n1.Load() != 2 {
		t.Errorf("a forced refresh after the throttle window should look up again, got %d", n1.Load())
	}
}

func TestEgress_FallsBackToSecondProvider(t *testing.T) {
	withEgressProviders(t,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		jsonReply(`{"success":true,"ip":"2001:db8::5","city":"Amsterdam","region":"North Holland","country_code":"NL","connection":{"isp":"Example BV"}}`))

	got := currentEgress(context.Background(), false)
	if got.IP != "2001:db8::5" || got.City != "Amsterdam" || got.Country != "NL" || got.Org != "Example BV" || got.Source != "ipwho.is" {
		t.Fatalf("unexpected fallback result %+v", got)
	}
}

func TestEgress_KeepsLastGoodResultOnFailure(t *testing.T) {
	var fail atomic.Bool
	withEgressProviders(t,
		func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			jsonReply(`{"ip":"203.0.113.7","city":"Berlin","country":"DE"}`)(w, r)
		},
		jsonReply(`{"success":false,"message":"rate limited"}`))

	if got := currentEgress(context.Background(), false); got.IP != "203.0.113.7" {
		t.Fatalf("first lookup failed: %+v", got)
	}
	fail.Store(true)
	egressMu.Lock()
	egressCache.CheckedAt = time.Now().Add(-egressTTL)
	egressMu.Unlock()
	got := currentEgress(context.Background(), false)
	if got.IP != "203.0.113.7" || got.Error == "" {
		t.Errorf("should keep the last known IP and flag the failure, got %+v", got)
	}
}

func TestHandleEgress(t *testing.T) {
	withEgressProviders(t, jsonReply(`{"ip":"203.0.113.9","city":"Munich","country":"DE"}`), jsonReply(`{}`))
	w := httptest.NewRecorder()
	handleEgress(w, httptest.NewRequest("GET", "/api/egress", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ip":"203.0.113.9"`) || !strings.Contains(w.Body.String(), `"city":"Munich"`) {
		t.Errorf("unexpected response %d %s", w.Code, w.Body)
	}
}

func TestEgress_RequiresLogin(t *testing.T) {
	withUsers(t)
	w := serve(requireAuth(okHandler()), httptest.NewRequest("GET", "/api/egress", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("the server's public IP must not be shown to anonymous visitors, got %d", w.Code)
	}
}
