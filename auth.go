package main

// auth.go — user accounts, browser sessions, API tokens and the middleware
// that puts the whole app (not just /admin) behind a login.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"mime"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	roleAdmin = "admin"
	roleUser  = "user"

	sessionCookieName = "leecher_session"
	apiTokenPrefix    = "lch_"
	maxTokensPerUser  = 20
)

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// dummyHash keeps failed logins for unknown users as slow as for known ones,
// so response timing does not reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("leecher-timing-equaliser"), bcrypt.DefaultCost)

// ---------------------------------------------------------------------------
// User store
// ---------------------------------------------------------------------------

type APIToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"hash"` // sha256 of the plaintext; the token itself is never stored
	Hint      string    `json:"hint"` // last 4 characters, for recognising tokens in the UI
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

type User struct {
	Username           string     `json:"username"`
	Hash               string     `json:"hash"`
	Role               string     `json:"role"`
	MustChangePassword bool       `json:"must_change_password,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	Tokens             []APIToken `json:"tokens,omitempty"`
}

func (u *User) clone() User {
	c := *u
	c.Tokens = append([]APIToken(nil), u.Tokens...)
	return c
}

type userStore struct {
	mu         sync.RWMutex
	path       string
	users      map[string]*User  // keyed by lower-cased username
	tokenIndex map[string]string // token hash -> user key
	fileMtime  time.Time         // users.json as last read or written by us
	fileSize   int64
}

var userDB = newUserStore("")

func newUserStore(path string) *userStore {
	return &userStore{path: path, users: map[string]*User{}, tokenIndex: map[string]string{}}
}

func userKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func (s *userStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var list []*User
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = map[string]*User{}
	s.tokenIndex = map[string]string{}
	s.rememberFileLocked()
	for _, u := range list {
		if u == nil || u.Username == "" {
			continue
		}
		k := userKey(u.Username)
		s.users[k] = u
		for _, t := range u.Tokens {
			s.tokenIndex[t.Hash] = k
		}
	}
	return nil
}

// saveLocked writes the store atomically. Callers must hold s.mu.
func (s *userStore) saveLocked() error {
	if s.path == "" {
		return nil // in-memory store (tests)
	}
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return userKey(list[i].Username) < userKey(list[j].Username) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.rememberFileLocked()
	return nil
}

func (s *userStore) rememberFileLocked() {
	if fi, err := os.Stat(s.path); err == nil {
		s.fileMtime, s.fileSize = fi.ModTime(), fi.Size()
	}
}

// reloadIfChanged picks up edits made by `leecher --add-user` or
// `--reset-password` while the server runs (e.g. via kubectl exec), so they
// take effect and are not overwritten by the server's next save.
func (s *userStore) reloadIfChanged() {
	if s.path == "" {
		return
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.RLock()
	same := fi.ModTime().Equal(s.fileMtime) && fi.Size() == s.fileSize
	s.mu.RUnlock()
	if same {
		return
	}
	if err := s.load(); err != nil {
		logger.Warn("Could not reload users.json", "error", err)
		return
	}
	logger.Info("Reloaded users.json after an external change", "users", s.count())
}

func (s *userStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

func (s *userStore) adminCountLocked() int {
	n := 0
	for _, u := range s.users {
		if u.Role == roleAdmin {
			n++
		}
	}
	return n
}

func (s *userStore) get(name string) (User, bool) {
	// Cheap stat; makes `--delete-user` / `--reset-password` lock out an
	// already signed-in session immediately.
	s.reloadIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[userKey(name)]
	if !ok {
		return User{}, false
	}
	return u.clone(), true
}

func (s *userStore) list() []User {
	s.reloadIfChanged()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u.clone())
	}
	sort.Slice(out, func(i, j int) bool { return userKey(out[i].Username) < userKey(out[j].Username) })
	return out
}

func validRole(role string) bool { return role == roleAdmin || role == roleUser }

func (s *userStore) create(name, password, role string, mustChange bool) error {
	name = strings.TrimSpace(name)
	if !usernameRe.MatchString(name) {
		return errors.New("Username must be 1-64 characters: letters, digits, . _ @ -")
	}
	if !validRole(role) {
		return errors.New("Role must be admin or user")
	}
	if err := validateAdminPassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return errors.New("Failed to hash password")
	}
	return s.insert(&User{Username: name, Hash: string(hash), Role: role, MustChangePassword: mustChange, CreatedAt: time.Now()})
}

// importHashed adds a user from an existing bcrypt hash (config migration).
func (s *userStore) importHashed(name, hash, role string) error {
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return fmt.Errorf("invalid bcrypt hash: %w", err)
	}
	mustChange := bcrypt.CompareHashAndPassword([]byte(hash), []byte("admin")) == nil
	return s.insert(&User{Username: strings.TrimSpace(name), Hash: hash, Role: role, MustChangePassword: mustChange, CreatedAt: time.Now()})
}

func (s *userStore) insert(u *User) error {
	s.reloadIfChanged()
	s.mu.Lock()
	defer s.mu.Unlock()
	k := userKey(u.Username)
	if _, exists := s.users[k]; exists {
		return errors.New("User already exists")
	}
	s.users[k] = u
	if err := s.saveLocked(); err != nil {
		delete(s.users, k)
		return fmt.Errorf("Failed to save users: %w", err)
	}
	return nil
}

func (s *userStore) verify(name, password string) (User, bool) {
	s.reloadIfChanged()
	s.mu.RLock()
	u, ok := s.users[userKey(name)]
	var hash []byte
	if ok {
		hash = []byte(u.Hash)
	} else {
		hash = dummyHash
	}
	s.mu.RUnlock()
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || !ok {
		return User{}, false
	}
	return s.get(name)
}

func (s *userStore) setPassword(name, password string, mustChange bool) error {
	if err := validateAdminPassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return errors.New("Failed to hash password")
	}
	s.reloadIfChanged()
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userKey(name)]
	if !ok {
		return errors.New("User not found")
	}
	oldHash, oldMust := u.Hash, u.MustChangePassword
	u.Hash, u.MustChangePassword = string(hash), mustChange
	if err := s.saveLocked(); err != nil {
		u.Hash, u.MustChangePassword = oldHash, oldMust
		return fmt.Errorf("Failed to save users: %w", err)
	}
	return nil
}

func (s *userStore) setRole(name, role string) error {
	s.reloadIfChanged()
	if !validRole(role) {
		return errors.New("Role must be admin or user")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userKey(name)]
	if !ok {
		return errors.New("User not found")
	}
	if u.Role == role {
		return nil
	}
	if u.Role == roleAdmin && s.adminCountLocked() <= 1 {
		return errors.New("Cannot demote the last admin")
	}
	old := u.Role
	u.Role = role
	if err := s.saveLocked(); err != nil {
		u.Role = old
		return fmt.Errorf("Failed to save users: %w", err)
	}
	return nil
}

func (s *userStore) remove(name string) error {
	s.reloadIfChanged()
	s.mu.Lock()
	defer s.mu.Unlock()
	k := userKey(name)
	u, ok := s.users[k]
	if !ok {
		return errors.New("User not found")
	}
	if u.Role == roleAdmin && s.adminCountLocked() <= 1 {
		return errors.New("Cannot delete the last admin")
	}
	delete(s.users, k)
	for _, t := range u.Tokens {
		delete(s.tokenIndex, t.Hash)
	}
	if err := s.saveLocked(); err != nil {
		s.users[k] = u
		for _, t := range u.Tokens {
			s.tokenIndex[t.Hash] = k
		}
		return fmt.Errorf("Failed to save users: %w", err)
	}
	return nil
}

// createToken returns the plaintext token exactly once; only its hash is kept.
func (s *userStore) createToken(name, label string) (string, APIToken, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "token"
	}
	if len(label) > 64 {
		return "", APIToken{}, errors.New("Token name too long (max 64)")
	}
	plain := apiTokenPrefix + generateSessionToken()
	tok := APIToken{
		ID:        generateID(),
		Name:      label,
		Hash:      hashToken(plain),
		Hint:      plain[len(plain)-4:],
		CreatedAt: time.Now(),
	}
	s.reloadIfChanged()
	s.mu.Lock()
	defer s.mu.Unlock()
	k := userKey(name)
	u, ok := s.users[k]
	if !ok {
		return "", APIToken{}, errors.New("User not found")
	}
	if len(u.Tokens) >= maxTokensPerUser {
		return "", APIToken{}, fmt.Errorf("At most %d tokens per user", maxTokensPerUser)
	}
	u.Tokens = append(u.Tokens, tok)
	s.tokenIndex[tok.Hash] = k
	if err := s.saveLocked(); err != nil {
		u.Tokens = u.Tokens[:len(u.Tokens)-1]
		delete(s.tokenIndex, tok.Hash)
		return "", APIToken{}, fmt.Errorf("Failed to save users: %w", err)
	}
	return plain, tok, nil
}

func (s *userStore) deleteToken(name, id string) error {
	s.reloadIfChanged()
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userKey(name)]
	if !ok {
		return errors.New("User not found")
	}
	for i, t := range u.Tokens {
		if t.ID != id {
			continue
		}
		u.Tokens = append(u.Tokens[:i:i], u.Tokens[i+1:]...)
		delete(s.tokenIndex, t.Hash)
		if err := s.saveLocked(); err != nil {
			return fmt.Errorf("Failed to save users: %w", err)
		}
		return nil
	}
	return errors.New("Token not found")
}

func (s *userStore) userForToken(plain string) (User, bool) {
	if !strings.HasPrefix(plain, apiTokenPrefix) {
		return User{}, false
	}
	h := hashToken(plain)
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.tokenIndex[h]
	if !ok {
		return User{}, false
	}
	u, ok := s.users[k]
	if !ok {
		return User{}, false
	}
	for i := range u.Tokens {
		if subtle.ConstantTimeCompare([]byte(u.Tokens[i].Hash), []byte(h)) != 1 {
			continue
		}
		// Persisting on every call would mean a disk write per request;
		// hourly granularity is plenty for "last used".
		if time.Since(u.Tokens[i].LastUsed) > time.Hour {
			u.Tokens[i].LastUsed = time.Now()
			if err := s.saveLocked(); err != nil {
				logger.Warn("Could not persist token last-used time", "error", err)
			}
		}
		return u.clone(), true
	}
	return User{}, false
}

// initUsers loads users.json. On first run it migrates the legacy single
// admin from config.json, or creates one from LEECHER_ADMIN_USER /
// LEECHER_ADMIN_PASSWORD.
func initUsers() {
	userDB = newUserStore(filepath.Join(stateDir(), "users.json"))
	if err := userDB.load(); err != nil && !os.IsNotExist(err) {
		logger.Error("Cannot load users", "path", userDB.path, "error", err)
		os.Exit(1)
	}
	if userDB.count() > 0 {
		logger.Info("Loaded users", "count", userDB.count())
		return
	}
	c := cfg()
	if c.AdminUser != "" && c.AdminHash != "" {
		if err := userDB.importHashed(c.AdminUser, c.AdminHash, roleAdmin); err != nil {
			logger.Error("Could not migrate admin from config.json", "error", err)
		} else {
			logger.Info("Migrated admin account from config.json", "user", c.AdminUser)
		}
		return
	}
	if u, p := os.Getenv("LEECHER_ADMIN_USER"), os.Getenv("LEECHER_ADMIN_PASSWORD"); u != "" && p != "" {
		if err := userDB.create(u, p, roleAdmin, false); err != nil {
			logger.Error("Could not create admin from environment", "error", err)
		} else {
			logger.Info("Created admin account from environment", "user", u)
		}
		return
	}
	logger.Warn("No user accounts exist — nobody can sign in. Create one with: leecher --add-user <name> <password> admin")
}

// runUserCLI handles the account-management flags. It returns false when the
// arguments are not a user command.
func runUserCLI(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "--add-user", "--reset-password", "--delete-user":
	default:
		return false
	}
	userDB = newUserStore(filepath.Join(stateDir(), "users.json"))
	if err := userDB.load(); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "cannot load users:", err)
		os.Exit(1)
	}
	var err error
	switch {
	case args[1] == "--add-user" && (len(args) == 4 || len(args) == 5):
		role := roleUser
		if len(args) == 5 {
			role = args[4]
		}
		err = userDB.create(args[2], args[3], role, false)
	case args[1] == "--reset-password" && len(args) == 4:
		err = userDB.setPassword(args[2], args[3], false)
	case args[1] == "--delete-user" && len(args) == 3:
		err = userDB.remove(args[2])
	default:
		fmt.Fprintln(os.Stderr, "usage: leecher --add-user <name> <password> [admin|user]\n       leecher --reset-password <name> <password>\n       leecher --delete-user <name>")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("ok:", userDB.path)
	os.Exit(0)
	return true
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

type session struct {
	User      string    `json:"user"`
	Expires   time.Time `json:"expires"`
	CSRFToken string    `json:"csrf"`
}

// sessions is keyed by sessionKey(cookie value) — a hash, so state/sessions.json
// (which keeps sign-ins across restarts) holds nothing a browser could present.
var (
	sessions        = make(map[string]*session)
	sessionsMu      sync.RWMutex
	sessionsPersist bool // set by loadSessions; off in tests
)

func sessionKey(token string) string { return hashToken(token) }

func sessionsPath() string { return filepath.Join(stateDir(), "sessions.json") }

// loadSessions restores unexpired sign-ins saved before a restart.
func loadSessions() {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	sessionsPersist = true
	data, err := os.ReadFile(sessionsPath())
	if err != nil {
		return
	}
	var saved map[string]*session
	if err := json.Unmarshal(data, &saved); err != nil {
		logger.Warn("Ignoring unreadable sessions file", "error", err)
		return
	}
	now := time.Now()
	for k, s := range saved {
		if s != nil && now.Before(s.Expires) {
			sessions[k] = s
		}
	}
	logger.Info("Restored sign-in sessions", "count", len(sessions))
}

// saveSessionsLocked writes the session table; callers hold sessionsMu.
func saveSessionsLocked() {
	if !sessionsPersist {
		return
	}
	data, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	os.MkdirAll(stateDir(), 0700)
	tmp := sessionsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logger.Warn("Could not save sessions", "error", err)
		return
	}
	os.Rename(tmp, sessionsPath())
}

func createSession(username string) (string, time.Time) {
	hours := cfg().SessionHours
	if hours <= 0 {
		hours = 72
	}
	token := generateSessionToken()
	expires := time.Now().Add(time.Duration(hours) * time.Hour)
	sessionsMu.Lock()
	sessions[sessionKey(token)] = &session{User: username, Expires: expires, CSRFToken: generateCSRFToken()}
	saveSessionsLocked()
	sessionsMu.Unlock()
	return token, expires
}

func lookupSession(token string) (*session, bool) {
	key := sessionKey(token)
	sessionsMu.RLock()
	s, ok := sessions[key]
	sessionsMu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(s.Expires) {
		sessionsMu.Lock()
		if cur, exists := sessions[key]; exists && time.Now().After(cur.Expires) {
			delete(sessions, key)
			saveSessionsLocked()
		}
		sessionsMu.Unlock()
		return nil, false
	}
	return s, true
}

// deleteSession signs one browser out.
func deleteSession(token string) {
	sessionsMu.Lock()
	if _, ok := sessions[sessionKey(token)]; ok {
		delete(sessions, sessionKey(token))
		saveSessionsLocked()
	}
	sessionsMu.Unlock()
}

// invalidateUserSessions drops every session of username except keep, so a
// password change or account deletion logs out other browsers.
func invalidateUserSessions(username, keep string) {
	k := userKey(username)
	keepKey := ""
	if keep != "" {
		keepKey = sessionKey(keep)
	}
	sessionsMu.Lock()
	changed := false
	for key, s := range sessions {
		if key != keepKey && userKey(s.User) == k {
			delete(sessions, key)
			changed = true
		}
	}
	if changed {
		saveSessionsLocked()
	}
	sessionsMu.Unlock()
}

func pruneExpiredSessions() {
	now := time.Now()
	sessionsMu.Lock()
	changed := false
	for key, s := range sessions {
		if now.After(s.Expires) {
			delete(sessions, key)
			changed = true
		}
	}
	if changed {
		saveSessionsLocked()
	}
	sessionsMu.Unlock()
}

func generateCSRFToken() string { return generateSessionToken() }

func csrfTokenForSession(sessionToken string) string {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	s, ok := sessions[sessionKey(sessionToken)]
	if !ok {
		return ""
	}
	if s.CSRFToken == "" {
		s.CSRFToken = generateCSRFToken()
		saveSessionsLocked()
	}
	return s.CSRFToken
}

// validateCSRF checks the X-CSRF-Token header (or the _csrf field of a
// urlencoded form) against the caller's session.
func validateCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	s, ok := lookupSession(cookie.Value)
	if !ok || s.CSRFToken == "" {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
			token = r.FormValue("_csrf")
		}
	}
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRFToken)) == 1
}

// isHTTPS reports whether the browser reached us over TLS, directly or via a
// trusted reverse proxy. Decides the cookie Secure flag.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return isTrustedProxy(host) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		// Lax (not Strict) so following a bookmarklet or shared link from
		// another site lands signed in; every state change needs a CSRF token.
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ---------------------------------------------------------------------------
// Request authentication
// ---------------------------------------------------------------------------

type authInfo struct {
	User         User
	ViaToken     bool
	SessionToken string
}

type authCtxKey struct{}

// resolveAuth identifies the caller from a bearer token or session cookie.
// The user record is re-read on every request so role changes and deletions
// take effect immediately.
func resolveAuth(r *http.Request) *authInfo {
	if h := r.Header.Get("Authorization"); h != "" {
		const p = "Bearer "
		if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
			if u, ok := userDB.userForToken(strings.TrimSpace(h[len(p):])); ok {
				return &authInfo{User: u, ViaToken: true}
			}
		}
		return nil
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	s, ok := lookupSession(cookie.Value)
	if !ok {
		return nil
	}
	u, ok := userDB.get(s.User)
	if !ok {
		return nil
	}
	return &authInfo{User: u, SessionToken: cookie.Value}
}

func currentAuth(r *http.Request) *authInfo {
	if a, ok := r.Context().Value(authCtxKey{}).(*authInfo); ok {
		return a
	}
	return resolveAuth(r)
}

func withAuth(r *http.Request, a *authInfo) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authCtxKey{}, a))
}

// isAdminAuthenticated is true for an admin using a browser session. API
// tokens never grant admin powers.
func isAdminAuthenticated(r *http.Request) bool {
	a := currentAuth(r)
	return a != nil && !a.ViaToken && a.User.Role == roleAdmin
}

// requestActor keys rate limits and per-user download slots by account when
// signed in, falling back to the client IP.
func requestActor(r *http.Request) string {
	if a := currentAuth(r); a != nil {
		return "user:" + userKey(a.User.Username)
	}
	return clientIP(r)
}

func isPublicPath(p string) bool {
	switch p {
	case "/login", "/admin/login", "/healthz", "/readyz", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

func isAdminPath(p string) bool {
	return p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/api/admin/")
}

// tokenAllowedPath limits bearer tokens to the download API, so a leaked
// token cannot manage accounts, settings or other tokens.
func tokenAllowedPath(p string) bool {
	for _, prefix := range []string{"/api/formats", "/api/download", "/api/jobs", "/api/files/", "/api/status/", "/api/cancel/", "/api/torrents", "/api/thumbnail/", "/downloads/", "/ws/status/"} {
		if p == prefix || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func wantsHTML(r *http.Request) bool {
	p := r.URL.Path
	return r.Method == http.MethodGet && !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/ws/") && !strings.HasPrefix(p, "/downloads/")
}

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// Already signed in at the SSO proxy: skip the local login form.
		if p == "/login" && r.Method == http.MethodGet && ssoFromRequest(r) != nil {
			if a, ok := applySSO(w, r, resolveAuth(r)); ok && a != nil {
				http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
				return
			}
		}
		if isPublicPath(p) {
			next.ServeHTTP(w, r)
			return
		}
		a, viaSSO := applySSO(w, r, resolveAuth(r))
		if a == nil {
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+urlpkg.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="leecher"`)
			jsonError(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		if a.ViaToken && !tokenAllowedPath(p) {
			jsonError(w, "API tokens cannot access this endpoint", http.StatusForbidden)
			return
		}
		if isAdminPath(p) && a.User.Role != roleAdmin {
			if wantsHTML(r) {
				http.Error(w, "Admins only", http.StatusForbidden)
				return
			}
			jsonError(w, "Admins only", http.StatusForbidden)
			return
		}
		if a.User.MustChangePassword && !a.ViaToken && !viaSSO && p != "/change-password" && p != "/logout" {
			if wantsHTML(r) {
				http.Redirect(w, r, "/change-password", http.StatusFound)
				return
			}
			jsonError(w, "Password change required", http.StatusForbidden)
			return
		}
		// Bearer callers skip CSRF: browsers never attach that header on
		// their own, so a cross-site page cannot ride on it.
		if !a.ViaToken && !isSafeMethod(r.Method) {
			if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
				r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			}
			if !validateCSRF(r) {
				jsonError(w, "Invalid CSRF token — reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, withAuth(r, a))
	})
}

// safeNext only allows same-site relative redirect targets after login.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	if u, err := urlpkg.Parse(next); err != nil || u.Host != "" || u.Scheme != "" || u.Path == "/login" {
		return "/"
	}
	return next
}

// ---------------------------------------------------------------------------
// Login / logout / forced password change
// ---------------------------------------------------------------------------

func renderLoginHTML(errorHTML, next string) []byte {
	html := loginHTML
	if html == "" {
		data, err := embedded.ReadFile("templates/login.html")
		if err != nil {
			return nil
		}
		html = strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML())
	}
	if errorHTML == "" && userDB.count() == 0 {
		errorHTML = `<div class="error">No accounts exist yet. Create one on the server with <code>leecher --add-user &lt;name&gt; &lt;password&gt; admin</code>.</div>`
	}
	html = strings.Replace(html, "<!--ERROR-->", errorHTML, 1)
	return []byte(strings.ReplaceAll(html, "<!--NEXT-->", htmlpkg.EscapeString(safeNext(next))))
}

func writeLogin(w http.ResponseWriter, code int, errorHTML, next string) {
	page := renderLoginHTML(errorHTML, next)
	if page == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(page)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		next := r.URL.Query().Get("next")
		if a := resolveAuth(r); a != nil && !a.ViaToken {
			http.Redirect(w, r, safeNext(next), http.StatusFound)
			return
		}
		writeLogin(w, http.StatusOK, "", next)
		return
	case http.MethodPost:
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r)
	if !limiter.allow(ip+":login", 5.0/60.0, 5) {
		logger.Warn("Login rate limited", "ip", ip)
		writeLogin(w, http.StatusTooManyRequests, `<div class="error">Too many attempts. Please wait a minute.</div>`, "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("username"))
	next := r.FormValue("next")
	// Per-account throttle on top of the per-IP one, against slow
	// distributed guessing of a single account.
	if !limiter.allow("login-user:"+userKey(name), 10.0/900.0, 10) {
		logger.Warn("Login throttled for account", "user", name, "ip", ip)
		writeLogin(w, http.StatusTooManyRequests, `<div class="error">Too many attempts for this account. Try again later.</div>`, next)
		return
	}
	u, ok := userDB.verify(name, r.FormValue("password"))
	if !ok {
		logger.Warn("Login failed", "user", name, "ip", ip)
		writeLogin(w, http.StatusUnauthorized, `<div class="error">Invalid username or password.</div>`, next)
		return
	}
	token, expires := createSession(u.Username)
	setSessionCookie(w, r, token, expires)
	logger.Info("Login successful", "user", u.Username, "ip", ip)
	if u.MustChangePassword {
		http.Redirect(w, r, "/change-password", http.StatusFound)
		return
	}
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

// handleLegacyAdminLogin keeps old bookmarks to /admin/login working.
func handleLegacyAdminLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login?next=%2Fadmin", http.StatusSeeOther)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validateCSRF(r) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		deleteSession(cookie.Value)
	}
	clearSessionCookie(w, r)
	if u := ssoLogoutURL(r); u != "" {
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

func handleChangePassword(w http.ResponseWriter, r *http.Request) {
	a := currentAuth(r)
	if a == nil || a.ViaToken {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	csrf := csrfTokenForSession(a.SessionToken)

	var errMsg string
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.FormValue("_csrf")), []byte(csrf)) != 1 {
			http.Error(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		newPass := r.FormValue("new_password")
		if newPass != r.FormValue("confirm_password") {
			errMsg = "Passwords do not match."
		} else if _, ok := userDB.verify(a.User.Username, r.FormValue("current_password")); !ok {
			errMsg = "Current password is incorrect."
		} else if newPass == r.FormValue("current_password") {
			errMsg = "The new password must differ from the current one."
		} else if err := userDB.setPassword(a.User.Username, newPass, false); err != nil {
			errMsg = err.Error()
		} else {
			invalidateUserSessions(a.User.Username, a.SessionToken)
			logger.Info("Password changed", "user", a.User.Username)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	errHTML := ""
	if errMsg != "" {
		errHTML = `<div class="error">` + htmlpkg.EscapeString(errMsg) + `</div>`
	}
	banner := "Choose a new password to continue."
	if a.User.MustChangePassword {
		banner = "For security, please choose a new password before you continue."
	}
	page := `<!DOCTYPE html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark">
<link rel="icon" type="image/svg+xml" href="/favicon.ico">
<link rel="stylesheet" href="/static/fonts.css">
<link rel="stylesheet" href="/static/style.css">
<link rel="stylesheet" href="/static/auth.css">
<title>Change password — Leecher</title>
</head><body class="auth-page">
<main class="auth-card">
<div class="banner">` + banner + `</div>
<h1>Change password</h1><p class="sub">Signed in as ` + htmlpkg.EscapeString(a.User.Username) + `</p>
` + errHTML + `
<form method="POST" action="/change-password">
<input type="hidden" name="_csrf" value="` + htmlpkg.EscapeString(csrf) + `">
<label class="label" for="cp_cur">Current password</label>
<input class="input" type="password" id="cp_cur" name="current_password" required autofocus autocomplete="current-password">
<label class="label" for="cp_new">New password</label>
<input class="input" type="password" id="cp_new" name="new_password" required minlength="8" autocomplete="new-password">
<label class="label" for="cp_conf">Repeat new password</label>
<input class="input" type="password" id="cp_conf" name="confirm_password" required minlength="8" autocomplete="new-password">
<button type="submit" class="btn btn-primary btn-lg">Save and continue</button>
</form></main></body></html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	w.Write([]byte(page))
}

// ---------------------------------------------------------------------------
// Account API (own profile, password, API tokens)
// ---------------------------------------------------------------------------

type tokenView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hint      string    `json:"hint"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

func tokenViews(ts []APIToken) []tokenView {
	out := make([]tokenView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tokenView{ID: t.ID, Name: t.Name, Hint: t.Hint, CreatedAt: t.CreatedAt, LastUsed: t.LastUsed})
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func handleAccountPage(w http.ResponseWriter, r *http.Request) {
	a := currentAuth(r)
	if a == nil || a.ViaToken {
		http.Redirect(w, r, "/login?next=%2Faccount", http.StatusFound)
		return
	}
	data, err := embedded.ReadFile("templates/account.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	page := strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML())
	page = strings.ReplaceAll(page, "<!--CSRF_TOKEN-->", htmlpkg.EscapeString(csrfTokenForSession(a.SessionToken)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(page))
}

func handleAccount(w http.ResponseWriter, r *http.Request) {
	a := currentAuth(r)
	if a == nil || a.ViaToken {
		jsonError(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	sub := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/account"), "/")

	switch {
	case sub == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"username": a.User.Username,
			"role":     a.User.Role,
			"tokens":   tokenViews(a.User.Tokens),
		})

	case sub == "password" && r.Method == http.MethodPost:
		if !adminWriteAllowed(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var p struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			jsonError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		if _, ok := userDB.verify(a.User.Username, p.CurrentPassword); !ok {
			jsonError(w, "Current password is incorrect", http.StatusForbidden)
			return
		}
		if err := userDB.setPassword(a.User.Username, p.NewPassword, false); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		invalidateUserSessions(a.User.Username, a.SessionToken)
		logger.Info("Password changed", "user", a.User.Username)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	case sub == "tokens" && r.Method == http.MethodPost:
		if !adminWriteAllowed(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var p struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			jsonError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		plain, tok, err := userDB.createToken(a.User.Username, p.Name)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info("API token created", "user", a.User.Username, "token", tok.Name)
		writeJSON(w, http.StatusOK, map[string]any{"token": plain, "id": tok.ID, "name": tok.Name})

	case strings.HasPrefix(sub, "tokens/") && r.Method == http.MethodDelete:
		if !adminWriteAllowed(w, r) {
			return
		}
		if err := userDB.deleteToken(a.User.Username, strings.TrimPrefix(sub, "tokens/")); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		logger.Info("API token revoked", "user", a.User.Username)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		jsonError(w, "Not found", http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------------
// Admin: user management
// ---------------------------------------------------------------------------

type userView struct {
	Username           string    `json:"username"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
	Tokens             int       `json:"tokens"`
}

func handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	me := currentAuth(r).User.Username
	name, _ := urlpkg.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/admin/users"), "/"))

	if r.Method != http.MethodGet && !adminWriteAllowed(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	switch {
	case name == "" && r.Method == http.MethodGet:
		list := userDB.list()
		out := make([]userView, 0, len(list))
		for _, u := range list {
			out = append(out, userView{Username: u.Username, Role: u.Role, MustChangePassword: u.MustChangePassword, CreatedAt: u.CreatedAt, Tokens: len(u.Tokens)})
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": out, "me": me})

	case name == "" && r.Method == http.MethodPost:
		var p struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			jsonError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		if p.Role == "" {
			p.Role = roleUser
		}
		// New accounts pick their own password on first sign-in.
		if err := userDB.create(p.Username, p.Password, p.Role, true); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info("User created", "by", me, "user", p.Username, "role", p.Role)
		writeJSON(w, http.StatusOK, map[string]string{"status": "created"})

	case name != "" && r.Method == http.MethodPost:
		var p struct {
			Role     string `json:"role"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			jsonError(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		if _, ok := userDB.get(name); !ok {
			jsonError(w, "User not found", http.StatusNotFound)
			return
		}
		if p.Role != "" {
			if userKey(name) == userKey(me) {
				jsonError(w, "You cannot change your own role", http.StatusBadRequest)
				return
			}
			if err := userDB.setRole(name, p.Role); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if p.Password != "" {
			if err := userDB.setPassword(name, p.Password, userKey(name) != userKey(me)); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			invalidateUserSessions(name, currentAuth(r).SessionToken)
		}
		logger.Info("User updated", "by", me, "user", name, "role_changed", p.Role != "", "password_reset", p.Password != "")
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})

	case name != "" && r.Method == http.MethodDelete:
		if userKey(name) == userKey(me) {
			jsonError(w, "You cannot delete your own account", http.StatusBadRequest)
			return
		}
		if err := userDB.remove(name); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		invalidateUserSessions(name, "")
		logger.Info("User deleted", "by", me, "user", name)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok"))
}
