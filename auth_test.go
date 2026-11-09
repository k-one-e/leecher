package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// withUsers swaps in an in-memory user store with one admin and one user.
func withUsers(t *testing.T) {
	t.Helper()
	orig := userDB
	userDB = newUserStore("")
	if err := userDB.create("alice", "alice-password", roleAdmin, false); err != nil {
		t.Fatal(err)
	}
	if err := userDB.create("bob", "bob-password", roleUser, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userDB = orig })
}

// loginAs creates a session and returns its cookie and CSRF token.
func loginAs(t *testing.T, name string) (*http.Cookie, string) {
	t.Helper()
	token, exp := createSession(name)
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, sessionKey(token))
		sessionsMu.Unlock()
	})
	return &http.Cookie{Name: sessionCookieName, Value: token, Expires: exp}, csrfTokenForSession(token)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := currentAuth(r); a != nil {
			w.Header().Set("X-User", a.User.Username)
		}
		w.WriteHeader(http.StatusOK)
	})
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestUserStore_CreateVerifyAndCaseInsensitivity(t *testing.T) {
	withUsers(t)
	if _, ok := userDB.verify("ALICE", "alice-password"); !ok {
		t.Error("username lookup should be case-insensitive")
	}
	if _, ok := userDB.verify("alice", "wrong-password"); ok {
		t.Error("wrong password must fail")
	}
	if _, ok := userDB.verify("nobody", "whatever-123"); ok {
		t.Error("unknown user must fail")
	}
	if err := userDB.create("Alice", "another-password", roleUser, false); err == nil {
		t.Error("duplicate usernames (case-insensitive) must be rejected")
	}
	for _, bad := range []string{"", "has space", "slash/name", strings.Repeat("x", 65)} {
		if err := userDB.create(bad, "valid-password", roleUser, false); err == nil {
			t.Errorf("username %q should be rejected", bad)
		}
	}
	if err := userDB.create("carol", "short", roleUser, false); err == nil {
		t.Error("short password should be rejected")
	}
	if err := userDB.create("carol", "valid-password", "root", false); err == nil {
		t.Error("unknown role should be rejected")
	}
}

func TestUserStore_LastAdminIsProtected(t *testing.T) {
	withUsers(t)
	if err := userDB.remove("alice"); err == nil {
		t.Error("deleting the only admin must fail")
	}
	if err := userDB.setRole("alice", roleUser); err == nil {
		t.Error("demoting the only admin must fail")
	}
	if err := userDB.setRole("bob", roleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := userDB.setRole("alice", roleUser); err != nil {
		t.Errorf("demoting one of two admins should work: %v", err)
	}
}

func TestUserStore_ImportFlagsDefaultPassword(t *testing.T) {
	orig := userDB
	userDB = newUserStore("")
	t.Cleanup(func() { userDB = orig })
	hash, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.MinCost)
	if err := userDB.importHashed("admin", string(hash), roleAdmin); err != nil {
		t.Fatal(err)
	}
	u, _ := userDB.get("admin")
	if !u.MustChangePassword {
		t.Error("an imported admin/admin account must be forced to change its password")
	}
}

func TestUserStore_Tokens(t *testing.T) {
	withUsers(t)
	plain, tok, err := userDB.createToken("bob", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, apiTokenPrefix) || tok.Hash == plain || strings.Contains(tok.Hash, plain) {
		t.Fatal("only a hash of the token may be stored")
	}
	u, ok := userDB.userForToken(plain)
	if !ok || u.Username != "bob" {
		t.Fatalf("token should resolve to bob, got %v %v", u.Username, ok)
	}
	if _, ok := userDB.userForToken(plain + "x"); ok {
		t.Error("altered token must not resolve")
	}
	if err := userDB.deleteToken("bob", tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := userDB.userForToken(plain); ok {
		t.Error("revoked token must not resolve")
	}
}

func TestRequireAuth_AnonymousIsRedirectedOrRejected(t *testing.T) {
	withUsers(t)
	h := requireAuth(okHandler())

	w := serve(h, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/login?next=%2F" {
		t.Errorf("GET / should redirect to login, got %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, p := range []string{"/api/formats", "/api/download", "/api/torrents", "/ws/status/x", "/downloads/x.mp4", "/api/admin/settings"} {
		w := serve(h, httptest.NewRequest("POST", p, nil))
		if p == "/ws/status/x" || p == "/downloads/x.mp4" {
			w = serve(h, httptest.NewRequest("GET", p, nil))
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s should be 401 for anonymous callers, got %d", p, w.Code)
		}
	}
	for _, p := range []string{"/login", "/static/app.js", "/favicon.ico", "/healthz", "/readyz"} {
		if w := serve(h, httptest.NewRequest("GET", p, nil)); w.Code != http.StatusOK {
			t.Errorf("%s should be public, got %d", p, w.Code)
		}
	}
}

func TestRequireAuth_RolesAndCSRF(t *testing.T) {
	withUsers(t)
	h := requireAuth(okHandler())
	bobCookie, bobCSRF := loginAs(t, "bob")
	aliceCookie, aliceCSRF := loginAs(t, "alice")

	req := httptest.NewRequest("GET", "/api/admin/settings", nil)
	req.AddCookie(bobCookie)
	if w := serve(h, req); w.Code != http.StatusForbidden {
		t.Errorf("users must not reach admin APIs, got %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/admin/settings", nil)
	req.AddCookie(aliceCookie)
	if w := serve(h, req); w.Code != http.StatusOK {
		t.Errorf("admins should reach admin APIs, got %d", w.Code)
	}

	req = httptest.NewRequest("POST", "/api/download", strings.NewReader("{}"))
	req.AddCookie(bobCookie)
	if w := serve(h, req); w.Code != http.StatusForbidden {
		t.Errorf("cookie POST without CSRF token must be rejected, got %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/api/download", strings.NewReader("{}"))
	req.AddCookie(bobCookie)
	req.Header.Set("X-CSRF-Token", aliceCSRF)
	if w := serve(h, req); w.Code != http.StatusForbidden {
		t.Errorf("another session's CSRF token must be rejected, got %d", w.Code)
	}
	req = httptest.NewRequest("POST", "/api/download", strings.NewReader("{}"))
	req.AddCookie(bobCookie)
	req.Header.Set("X-CSRF-Token", bobCSRF)
	w := serve(h, req)
	if w.Code != http.StatusOK || w.Header().Get("X-User") != "bob" {
		t.Errorf("valid session + CSRF should pass as bob, got %d %q", w.Code, w.Header().Get("X-User"))
	}

	// Urlencoded forms (logout) may carry the token as _csrf.
	req = httptest.NewRequest("POST", "/logout", strings.NewReader("_csrf="+url.QueryEscape(bobCSRF)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(bobCookie)
	if w := serve(h, req); w.Code != http.StatusOK {
		t.Errorf("form _csrf should be accepted, got %d", w.Code)
	}
}

func TestRequireAuth_DeletedUserLosesSession(t *testing.T) {
	withUsers(t)
	h := requireAuth(okHandler())
	cookie, _ := loginAs(t, "bob")
	if err := userDB.remove("bob"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/torrents", nil)
	req.AddCookie(cookie)
	if w := serve(h, req); w.Code != http.StatusUnauthorized {
		t.Errorf("a deleted user's session must stop working, got %d", w.Code)
	}
}

func TestRequireAuth_BearerTokens(t *testing.T) {
	withUsers(t)
	h := requireAuth(okHandler())
	plain, _, _ := userDB.createToken("alice", "script")

	do := func(method, path string, token string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		return serve(h, req).Code
	}
	if c := do("POST", "/api/download", plain); c != http.StatusOK {
		t.Errorf("token should start downloads without CSRF, got %d", c)
	}
	if c := do("GET", "/api/status/abc", plain); c != http.StatusOK {
		t.Errorf("token should read job status, got %d", c)
	}
	// Even an admin's token must not reach settings, users or token management.
	for _, p := range []string{"/api/admin/settings", "/api/admin/users", "/api/account/tokens", "/admin"} {
		if c := do("POST", p, plain); c != http.StatusForbidden {
			t.Errorf("token must not reach %s, got %d", p, c)
		}
	}
	if c := do("POST", "/api/download", "lch_bogus"); c != http.StatusUnauthorized {
		t.Errorf("unknown token should be 401, got %d", c)
	}
}

func TestRequireAuth_ForcedPasswordChange(t *testing.T) {
	withUsers(t)
	if err := userDB.setPassword("bob", "temp-password", true); err != nil {
		t.Fatal(err)
	}
	h := requireAuth(okHandler())
	cookie, _ := loginAs(t, "bob")
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	w := serve(h, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/change-password" {
		t.Errorf("must be sent to /change-password, got %d %q", w.Code, w.Header().Get("Location"))
	}
	req = httptest.NewRequest("GET", "/change-password", nil)
	req.AddCookie(cookie)
	if w := serve(h, req); w.Code != http.StatusOK {
		t.Errorf("/change-password itself must stay reachable, got %d", w.Code)
	}
}

func TestLogin_SetsSessionAndHonoursSafeNext(t *testing.T) {
	withUsers(t)
	oldLimiter := limiter
	limiter = newRateLimiter()
	t.Cleanup(func() { limiter = oldLimiter })

	form := url.Values{"username": {"bob"}, "password": {"bob-password"}, "next": {"/account"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handleLogin(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/account" {
		t.Fatalf("expected redirect to /account, got %d %q", w.Code, w.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected an HttpOnly, SameSite=Lax session cookie, got %+v", cookie)
	}
	if s, ok := lookupSession(cookie.Value); !ok || s.User != "bob" {
		t.Error("session should belong to bob")
	}

	form.Set("password", "nope-nope")
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handleLogin(w, req)
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Errorf("bad password must not create a session, got %d", w.Code)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                      "/",
		"/":                     "/",
		"/account":              "/account",
		"/?url=https%3A%2F%2Fx": "/?url=https%3A%2F%2Fx",
		"//evil.com":            "/",
		`/\evil.com`:            "/",
		"https://evil.com":      "/",
		"javascript:alert(1)":   "/",
		"/login":                "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsHTTPSOnlyTrustsConfiguredProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.42.0.5:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	if isHTTPS(req) {
		t.Error("X-Forwarded-Proto from an untrusted peer must be ignored")
	}
	old := trustedProxies
	trustedProxies = parseTrustedProxies([]string{"10.42.0.0/16"})
	t.Cleanup(func() { trustedProxies = old })
	if !isHTTPS(req) {
		t.Error("X-Forwarded-Proto from a trusted proxy should be honoured")
	}
}

func TestJobAndFileAccessFollowOwnership(t *testing.T) {
	withUsers(t)
	if err := userDB.create("carol", "carol-password", roleUser, false); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "ownjob", Status: "done", Token: "secret", Owner: "bob", Filename: "ownjob_file.mp4", subscribers: map[chan struct{}]struct{}{}}
	jobsMu.Lock()
	jobs[job.ID] = job
	jobsMu.Unlock()
	t.Cleanup(func() {
		jobsMu.Lock()
		delete(jobs, job.ID)
		jobsMu.Unlock()
	})

	as := func(name string) *http.Request {
		u, _ := userDB.get(name)
		return withAuth(httptest.NewRequest("GET", "/", nil), &authInfo{User: u})
	}
	if !authorizeJob(as("bob"), job) || !canAccessFile(as("bob"), job.Filename) {
		t.Error("the owner should see their job and file")
	}
	if !authorizeJob(as("alice"), job) || !canAccessFile(as("alice"), job.Filename) {
		t.Error("admins should see every job and file")
	}
	if authorizeJob(as("carol"), job) || canAccessFile(as("carol"), job.Filename) {
		t.Error("other users must not see the job or file")
	}
	if canAccessFile(as("carol"), "unknown-file.mp4") {
		t.Error("files without a known owner are admin-only")
	}
}

func TestAdminUsersAPI(t *testing.T) {
	withUsers(t)
	oldLimiter := limiter
	limiter = newRateLimiter()
	t.Cleanup(func() { limiter = oldLimiter })
	alice, _ := userDB.get("alice")
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = withAuth(req, &authInfo{User: alice})
		w := httptest.NewRecorder()
		handleAdminUsers(w, req)
		return w
	}
	if w := call("POST", "/api/admin/users", `{"username":"dave","password":"dave-password","role":"user"}`); w.Code != 200 {
		t.Fatalf("create failed: %d %s", w.Code, w.Body)
	}
	if u, _ := userDB.get("dave"); !u.MustChangePassword {
		t.Error("admin-created accounts must change their password on first sign-in")
	}
	if w := call("DELETE", "/api/admin/users/alice", ""); w.Code != http.StatusBadRequest {
		t.Errorf("admins must not delete themselves, got %d", w.Code)
	}
	if w := call("POST", "/api/admin/users/alice", `{"role":"user"}`); w.Code != http.StatusBadRequest {
		t.Errorf("admins must not change their own role, got %d", w.Code)
	}
	if w := call("DELETE", "/api/admin/users/dave", ""); w.Code != 200 {
		t.Errorf("delete failed: %d", w.Code)
	}
	w := call("GET", "/api/admin/users", "")
	var resp struct {
		Users []userView `json:"users"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Users) != 2 {
		t.Errorf("expected 2 users, got %d", len(resp.Users))
	}
	for _, u := range resp.Users {
		if strings.Contains(w.Body.String(), "$2a$") {
			t.Fatalf("password hashes must never be returned (user %s)", u.Username)
		}
	}
}

func TestSessionExpiry(t *testing.T) {
	sessionsMu.Lock()
	sessions[sessionKey("expired")] = &session{User: "x", Expires: time.Now().Add(-time.Minute)}
	sessionsMu.Unlock()
	if _, ok := lookupSession("expired"); ok {
		t.Error("expired sessions must not resolve")
	}
	sessionsMu.RLock()
	_, still := sessions[sessionKey("expired")]
	sessionsMu.RUnlock()
	if still {
		t.Error("expired sessions should be removed on lookup")
	}
}

func TestUserStore_PicksUpExternalEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	server := newUserStore(path)
	if err := server.create("alice", "alice-password", roleAdmin, false); err != nil {
		t.Fatal(err)
	}
	// A separate process (the CLI) edits the file while the server runs.
	cli := newUserStore(path)
	if err := cli.load(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // distinct mtime on coarse filesystems
	if err := cli.create("rescue", "rescue-password", roleAdmin, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.verify("rescue", "rescue-password"); !ok {
		t.Fatal("the server should see a user added by the CLI")
	}
	// A later server-side save must keep the CLI's user.
	if err := server.create("bob", "bob-password", roleUser, false); err != nil {
		t.Fatal(err)
	}
	again := newUserStore(path)
	if err := again.load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := again.get("rescue"); !ok {
		t.Error("the CLI's user was lost by a later server save")
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	t.Setenv("LEECHER_HOME", t.TempDir())
	sessionsMu.Lock()
	oldSessions, oldPersist := sessions, sessionsPersist
	sessions, sessionsPersist = map[string]*session{}, true
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		sessions, sessionsPersist = oldSessions, oldPersist
		sessionsMu.Unlock()
	})

	token, _ := createSession("alice")
	gone, _ := createSession("bob")
	deleteSession(gone)
	csrf := csrfTokenForSession(token)

	data, err := os.ReadFile(sessionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("the raw session token must never be written to disk")
	}

	// Simulate a restart: empty memory, load from disk.
	sessionsMu.Lock()
	sessions = map[string]*session{}
	sessionsMu.Unlock()
	loadSessions()

	s, ok := lookupSession(token)
	if !ok || s.User != "alice" || s.CSRFToken != csrf {
		t.Fatalf("sign-in should survive a restart, got %+v %v", s, ok)
	}
	if _, ok := lookupSession(gone); ok {
		t.Error("a signed-out session must not come back")
	}
	invalidateUserSessions("alice", "")
	sessionsMu.Lock()
	sessions = map[string]*session{}
	sessionsMu.Unlock()
	loadSessions()
	if _, ok := lookupSession(token); ok {
		t.Error("invalidated sessions must stay gone after a restart")
	}
}

// The shipped example config must not create an account: install.sh copies it
// to config.json, and a known admin password would let anyone who reaches a
// fresh install first take it over.
func TestConfigExampleHasNoCredentials(t *testing.T) {
	data, err := os.ReadFile("config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var example map[string]interface{}
	if err := json.Unmarshal(data, &example); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"admin_user", "admin_hash"} {
		if value, _ := example[key].(string); value != "" {
			t.Errorf("config.json.example sets %s; leave it empty", key)
		}
	}
}
