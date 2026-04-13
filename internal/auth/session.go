// Package auth provides OIDC authentication and session management.
package auth

import (
	"encoding/gob"
	"fmt"
	"net/http"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
)

const (
	sessionName             = "cozytempl"
	sessionKeyToken         = "id_token"
	sessionKeyRefreshToken  = "refresh_token"
	sessionKeyUser          = "username"
	sessionKeyGroups        = "groups"
	sessionKeyKubeconfig    = "kubeconfig"
	sessionKeyBearerToken   = "bearer_token"    //nolint:gosec // session map key, not a credential
	sessionKeyIDTokenExpiry = "id_token_expiry" //nolint:gosec // session map key, not a credential

	aes256KeySize     = 32
	sessionMaxAgeSecs = 86400
)

// SessionStore wraps gorilla/sessions for encrypted cookie storage.
type SessionStore struct {
	store *sessions.CookieStore
}

// NewSessionStore creates a session store with the given secret key.
func NewSessionStore(secret string) *SessionStore {
	// Register []string so gorilla/sessions can serialize it via gob.
	gob.Register([]string{})

	hashKey := securecookie.GenerateRandomKey(aes256KeySize)
	blockKey := padOrTruncate([]byte(secret), aes256KeySize)

	store := sessions.NewCookieStore(hashKey, blockKey)
	// Cookie hardening:
	//   - HttpOnly: JavaScript cannot read the session cookie, so an
	//     XSS in a future dependency can't exfiltrate it.
	//   - Secure: the cookie is only sent over HTTPS. Production runs
	//     behind a TLS-terminating proxy (Cloudflare Tunnel, nginx)
	//     so the browser always sees an HTTPS origin.
	//   - SameSite=Lax: blocks cross-site POST and XHR/fetch, which is
	//     the entire CSRF attack surface for an htmx app. We intentionally
	//     do NOT issue per-form CSRF tokens — Lax carries that weight.
	//     Every modern browser (Chrome 80+, Firefox, Safari, Edge)
	//     enforces Lax-blocks-cross-site-POST; pre-2020 browsers do not,
	//     and we accept that as out-of-scope.
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   sessionMaxAgeSecs,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}

	return &SessionStore{store: store}
}

func padOrTruncate(key []byte, size int) []byte {
	if len(key) >= size {
		return key[:size]
	}

	padded := make([]byte, size)
	copy(padded, key)

	return padded
}

// Get returns the session for the given request.
func (sst *SessionStore) Get(req *http.Request) (*sessions.Session, error) {
	session, err := sst.store.Get(req, sessionName)
	if err != nil {
		return nil, fmt.Errorf("getting session: %w", err)
	}

	return session, nil
}

// Save persists the session.
func (sst *SessionStore) Save(
	req *http.Request,
	writer http.ResponseWriter,
	session *sessions.Session,
) error {
	err := sst.store.Save(req, writer, session)
	if err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	return nil
}

// UserSession bundles everything the middleware needs to reconstruct a
// UserContext from a cookie. It is *not* stored as a single gob blob —
// each field has its own session key so the cookie format stays
// backwards-compatible with existing sessions and any individual field
// can be refreshed without touching the others.
type UserSession struct {
	Username      string
	Groups        []string
	IDToken       string
	RefreshToken  string
	IDTokenExpiry int64 // Unix seconds; 0 means unknown / not applicable
}

// SetUser stores the user info in the session. Pass empty strings / zero
// values for fields that do not apply to the current auth mode (e.g.
// dev mode sets everything blank; passthrough fills all five).
func SetUser(session *sessions.Session, info *UserSession) {
	session.Values[sessionKeyUser] = info.Username
	session.Values[sessionKeyGroups] = info.Groups
	session.Values[sessionKeyToken] = info.IDToken
	session.Values[sessionKeyRefreshToken] = info.RefreshToken
	session.Values[sessionKeyIDTokenExpiry] = info.IDTokenExpiry
}

// GetUser retrieves the user info from the session. Missing fields come
// back as their zero value so callers can treat the result as a
// best-effort snapshot and check Username for the is-authenticated signal.
func GetUser(session *sessions.Session) UserSession {
	var info UserSession

	if val, ok := session.Values[sessionKeyUser].(string); ok {
		info.Username = val
	}

	if val, ok := session.Values[sessionKeyGroups].([]string); ok {
		info.Groups = val
	}

	if val, ok := session.Values[sessionKeyToken].(string); ok {
		info.IDToken = val
	}

	if val, ok := session.Values[sessionKeyRefreshToken].(string); ok {
		info.RefreshToken = val
	}

	if val, ok := session.Values[sessionKeyIDTokenExpiry].(int64); ok {
		info.IDTokenExpiry = val
	}

	return info
}

// SetKubeconfig stores the user's uploaded kubeconfig bytes in the
// session. Used only in byok mode.
func SetKubeconfig(session *sessions.Session, kubeconfig []byte) {
	session.Values[sessionKeyKubeconfig] = kubeconfig
}

// GetKubeconfig retrieves the stored kubeconfig bytes from the session.
// The second return is false when nothing is stored or the slice is empty.
func GetKubeconfig(session *sessions.Session) ([]byte, bool) {
	val, ok := session.Values[sessionKeyKubeconfig].([]byte)
	if !ok || len(val) == 0 {
		return nil, false
	}

	return val, true
}

// SetBearerToken stores a Kubernetes Bearer token in the session.
// Used only in token mode, where the token IS the user's identity and
// every k8s call uses it as the Bearer credential.
func SetBearerToken(session *sessions.Session, token string) {
	session.Values[sessionKeyBearerToken] = token
}

// GetBearerToken retrieves the stored Bearer token from the session.
// The second return is false when nothing is stored or the value is empty.
func GetBearerToken(session *sessions.Session) (string, bool) {
	val, ok := session.Values[sessionKeyBearerToken].(string)
	if !ok || val == "" {
		return "", false
	}

	return val, true
}

// Clear removes all user data from the session.
func Clear(session *sessions.Session) {
	delete(session.Values, sessionKeyUser)
	delete(session.Values, sessionKeyGroups)
	delete(session.Values, sessionKeyToken)
	delete(session.Values, sessionKeyRefreshToken)
	delete(session.Values, sessionKeyIDTokenExpiry)
	delete(session.Values, sessionKeyKubeconfig)
	delete(session.Values, sessionKeyBearerToken)
	session.Options.MaxAge = -1
}
