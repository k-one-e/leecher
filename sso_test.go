package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withSSO(t *testing.T) {
	t.Helper()
	withUsers(t)
	withConfig(t, func(c *Config) {
		c.SSOUserHeader = "Remote-User"
		c.SSOGroupsHeader = "Remote-Groups"
		c.SSOAdminGroup = "admins"
		c.SSOLogoutURL = "https://auth.{domain}/logout"
	})
	old := trustedProxies
	trustedProxies = parseTrustedProxies([]string{"10.42.0.0/16"})
	t.Cleanup(func() { trustedProxies = old })
}

func ssoRequest(method, path, user, groups string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "10.42.0.9:5555"
	req.Header.Set("Remote-User", user)
	if groups != "" {
		req.Header.Set("Remote-Groups", groups)
	}
	return req
}

func sessionCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func TestSSO_ProvisionsUserAndIssuesSession(t *testing.T) {
	withSSO(t)
	h := requireAuth(okHandler())

	w := serve(h, ssoRequest("GET", "/", "carol", "users"))
	if w.Code != http.StatusOK || w.Header().Get("X-User") != "carol" {
		t.Fatalf("want carol signed in, got %d %q", w.Code, w.Header().Get("X-User"))
	}
	u, ok := userDB.get("carol")
	if !ok || u.Role != roleUser || u.MustChangePassword {
		t.Fatalf("unexpected account %+v", u)
	}
	c := sessionCookie(w)
	if c == nil {
		t.Fatal("no session cookie issued")
	}

	// The issued session carries a CSRF token, so POSTs work.
	post := ssoRequest("POST", "/api/jobs/clear", "carol", "users")
	post.AddCookie(c)
	post.Header.Set("X-CSRF-Token", csrfTokenForSession(c.Value))
	if w := serve(h, post); w.Code != http.StatusOK {
		t.Fatalf("POST with SSO session: %d", w.Code)
	}
	// No new session for the same user.
	if sessionCookie(serve(h, func() *http.Request { r := ssoRequest("GET", "/", "carol", "users"); r.AddCookie(c); return r }())) != nil {
		t.Fatal("re-issued a session for an existing one")
	}
}

func TestSSO_IgnoresUntrustedPeer(t *testing.T) {
	withSSO(t)
	req := ssoRequest("GET", "/api/jobs", "alice", "admins")
	req.RemoteAddr = "203.0.113.7:4444"
	if w := serve(requireAuth(okHandler()), req); w.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed header from untrusted peer: want 401, got %d", w.Code)
	}
	if _, ok := userDB.get("mallory"); ok {
		t.Fatal("unexpected user")
	}
}

func TestSSO_DisabledByDefault(t *testing.T) {
	withUsers(t)
	old := trustedProxies
	trustedProxies = parseTrustedProxies([]string{"10.42.0.0/16"})
	t.Cleanup(func() { trustedProxies = old })
	if w := serve(requireAuth(okHandler()), ssoRequest("GET", "/api/jobs", "alice", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("SSO off: want 401, got %d", w.Code)
	}
}

func TestSSO_RoleFollowsGroups(t *testing.T) {
	withSSO(t)
	h := requireAuth(okHandler())
	serve(h, ssoRequest("GET", "/", "bob", "admins,users"))
	if u, _ := userDB.get("bob"); u.Role != roleAdmin {
		t.Fatalf("bob in admins: role %q", u.Role)
	}
	serve(h, ssoRequest("GET", "/", "bob", "users"))
	if u, _ := userDB.get("bob"); u.Role != roleUser {
		t.Fatalf("bob left admins: role %q", u.Role)
	}
	// The last admin is never demoted by a group change.
	serve(h, ssoRequest("GET", "/", "alice", "users"))
	if u, _ := userDB.get("alice"); u.Role != roleAdmin {
		t.Fatalf("last admin demoted: role %q", u.Role)
	}
}

func TestSSO_SwitchesSessionToProxyUser(t *testing.T) {
	withSSO(t)
	cookie, _ := loginAs(t, "bob")
	req := ssoRequest("GET", "/", "alice", "admins")
	req.AddCookie(cookie)
	w := serve(requireAuth(okHandler()), req)
	if w.Header().Get("X-User") != "alice" || sessionCookie(w) == nil {
		t.Fatalf("want a fresh alice session, got user %q", w.Header().Get("X-User"))
	}
}

func TestSSO_BearerTokenWins(t *testing.T) {
	withSSO(t)
	token, _, err := userDB.createToken("bob", "script")
	if err != nil {
		t.Fatal(err)
	}
	req := ssoRequest("GET", "/api/jobs", "alice", "admins")
	req.Header.Set("Authorization", "Bearer "+token)
	w := serve(requireAuth(okHandler()), req)
	if w.Header().Get("X-User") != "bob" || sessionCookie(w) != nil {
		t.Fatalf("token request: user %q, cookie %v", w.Header().Get("X-User"), sessionCookie(w))
	}
}

func TestSSO_LoginPageRedirectsAndLogoutGoesToProxy(t *testing.T) {
	withSSO(t)
	h := requireAuth(okHandler())
	w := serve(h, ssoRequest("GET", "/login?next=/account", "alice", "admins"))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/account" {
		t.Fatalf("login with SSO: %d → %q", w.Code, w.Header().Get("Location"))
	}

	cookie, csrf := loginAs(t, "alice")
	req := httptest.NewRequest("POST", "/logout", strings.NewReader("_csrf="+csrf))
	req.Host = "leecher.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	handleLogout(w, req)
	if loc := w.Header().Get("Location"); loc != "https://auth.example.com/logout" {
		t.Fatalf("logout redirect %q", loc)
	}
}
