package main

// sso.go — single sign-on behind a forward-auth proxy (Traefik + Authelia).
//
// The proxy authenticates the browser and passes the user in a header such as
// Remote-User. Leecher maps that to a local account (creating it on first
// visit) and issues its normal session cookie, so CSRF tokens, the admin page
// and API tokens keep working unchanged.

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

type ssoIdentity struct {
	Username string
	Groups   []string
}

// ssoFromRequest returns the identity asserted by the proxy, or nil when SSO
// is off, the header is missing, or the request did not come from a trusted
// proxy (anyone else could simply send the header).
func ssoFromRequest(r *http.Request) *ssoIdentity {
	c := cfg()
	if c.SSOUserHeader == "" {
		return nil
	}
	name := strings.TrimSpace(r.Header.Get(c.SSOUserHeader))
	if name == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host) {
		return nil
	}
	id := &ssoIdentity{Username: name}
	if c.SSOGroupsHeader != "" {
		for _, g := range strings.Split(r.Header.Get(c.SSOGroupsHeader), ",") {
			if g = strings.TrimSpace(g); g != "" {
				id.Groups = append(id.Groups, g)
			}
		}
	}
	return id
}

// ssoRole is the role the proxy's groups grant, or "" when roles are managed
// in the admin panel.
func ssoRole(id *ssoIdentity) string {
	admin := cfg().SSOAdminGroup
	if admin == "" {
		return ""
	}
	for _, g := range id.Groups {
		if strings.EqualFold(g, admin) {
			return roleAdmin
		}
	}
	return roleUser
}

// ssoUser returns the local account for id, creating it on first sign-in and
// keeping its role in line with the proxy's groups.
func ssoUser(id *ssoIdentity) (User, error) {
	role := ssoRole(id)
	if u, ok := userDB.get(id.Username); ok {
		if role != "" && u.Role != role {
			if err := userDB.setRole(u.Username, role); err != nil {
				// e.g. "Cannot demote the last admin": keep the current role.
				logger.Warn("SSO role sync skipped", "user", u.Username, "role", role, "error", err)
			} else {
				u.Role = role
			}
		}
		return u, nil
	}
	if role == "" {
		role = roleUser
	}
	// The account is only reachable through SSO; nobody knows this password.
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return User{}, err
	}
	if err := userDB.create(id.Username, hex.EncodeToString(buf), role, false); err != nil {
		return User{}, err
	}
	logger.Info("Created account from SSO", "user", id.Username, "role", role)
	u, _ := userDB.get(id.Username)
	return u, nil
}

// applySSO makes sure a browser signed in at the proxy has a matching leecher
// session. It returns the auth info to use and whether SSO identified the
// caller. Bearer-token requests are left alone.
func applySSO(w http.ResponseWriter, r *http.Request, a *authInfo) (*authInfo, bool) {
	if a != nil && a.ViaToken {
		return a, false
	}
	id := ssoFromRequest(r)
	if id == nil {
		return a, false
	}
	u, err := ssoUser(id)
	if err != nil {
		logger.Warn("SSO sign-in failed", "user", id.Username, "error", err)
		return a, false
	}
	if a != nil && userKey(a.User.Username) == userKey(u.Username) {
		a.User = u
		return a, true
	}
	// No session yet, or one for a different account: switch to the SSO user.
	token, expires := createSession(u.Username)
	setSessionCookie(w, r, token, expires)
	replaceRequestCookie(r, sessionCookieName, token)
	return &authInfo{User: u, SessionToken: token}, true
}

// replaceRequestCookie rewrites the Cookie header so handlers later in this
// request (CSRF checks, page rendering) see the new session.
func replaceRequestCookie(r *http.Request, name, value string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
	r.AddCookie(&http.Cookie{Name: name, Value: value})
}

// ssoLogoutURL is where to send the browser after signing out, or "" to use
// the local login page.
func ssoLogoutURL(r *http.Request) string {
	u := cfg().SSOLogoutURL
	if u == "" || cfg().SSOUserHeader == "" {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	domain := host
	if i := strings.IndexByte(host, '.'); i >= 0 {
		domain = host[i+1:]
	}
	return strings.ReplaceAll(u, "{domain}", domain)
}
