package auth

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/gorilla/sessions"
)

const stateKeyName = "oauth_state"

// Handler provides HTTP handlers for the OIDC authentication flow.
type Handler struct {
	oidc  *OIDCProvider
	store *SessionStore
	log   *slog.Logger
}

// NewHandler creates an auth handler.
func NewHandler(oidc *OIDCProvider, store *SessionStore, log *slog.Logger) *Handler {
	return &Handler{
		oidc:  oidc,
		store: store,
		log:   log,
	}
}

// HandleLogin redirects the user to the OIDC provider for authentication.
func (hnd *Handler) HandleLogin(writer http.ResponseWriter, req *http.Request) {
	state, err := generateState()
	if err != nil {
		hnd.log.Error("generating oauth state", "error", err)
		http.Error(writer, `{"error":"internal error"}`, http.StatusInternalServerError)

		return
	}

	session, err := hnd.store.Get(req)
	if err != nil {
		hnd.log.Error("getting session for login", "error", err)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	session.Values[stateKeyName] = state

	err = hnd.store.Save(req, writer, session)
	if err != nil {
		hnd.log.Error("saving session for login", "error", err)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	http.Redirect(writer, req, hnd.oidc.AuthCodeURL(state), http.StatusFound)
}

// HandleCallback processes the OIDC callback after authentication.
func (hnd *Handler) HandleCallback(writer http.ResponseWriter, req *http.Request) {
	session, ok := hnd.validateCallbackState(writer, req)
	if !ok {
		return
	}

	code := req.URL.Query().Get("code")
	if code == "" {
		http.Error(writer, `{"error":"missing authorization code"}`, http.StatusBadRequest)

		return
	}

	result, err := hnd.oidc.Exchange(req.Context(), code)
	if err != nil {
		hnd.log.Error("exchanging auth code", "error", err)
		http.Error(writer, `{"error":"authentication failed"}`, http.StatusInternalServerError)

		return
	}

	expiry, expErr := IDTokenExpiry(result.IDToken)
	if expErr != nil {
		hnd.log.Warn("parsing id token expiry at callback", "error", expErr)
	}

	SetUser(session, &UserSession{
		Username:      result.Claims.Username,
		Groups:        result.Claims.Groups,
		IDToken:       result.IDToken,
		RefreshToken:  result.RefreshToken,
		IDTokenExpiry: expiry.Unix(),
	})

	err = hnd.store.Save(req, writer, session)
	if err != nil {
		hnd.log.Error("saving session after callback", "error", err)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	hnd.log.Info("user authenticated", "username", result.Claims.Username)
	http.Redirect(writer, req, "/", http.StatusFound)
}

// HandleLogout clears the session and redirects to the login page.
func (hnd *Handler) HandleLogout(writer http.ResponseWriter, req *http.Request) {
	session, err := hnd.store.Get(req)
	if err != nil {
		hnd.log.Error("getting session for logout", "error", err)
		http.Redirect(writer, req, "/auth/login", http.StatusFound)

		return
	}

	Clear(session)

	err = hnd.store.Save(req, writer, session)
	if err != nil {
		hnd.log.Error("saving session for logout", "error", err)
	}

	http.Redirect(writer, req, "/auth/login", http.StatusFound)
}

// validateCallbackState verifies the OAuth state nonce and returns the
// session on success. A false second return means a response has
// already been written and the caller must bail out. Pulled out so
// HandleCallback stays under the funlen budget.
func (hnd *Handler) validateCallbackState(writer http.ResponseWriter, req *http.Request) (*sessions.Session, bool) {
	session, err := hnd.store.Get(req)
	if err != nil {
		hnd.log.Error("getting session for callback", "error", err)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return nil, false
	}

	expectedState, _ := session.Values[stateKeyName].(string)
	actualState := req.URL.Query().Get("state")

	if expectedState == "" || actualState != expectedState {
		http.Error(writer, `{"error":"invalid oauth state"}`, http.StatusBadRequest)

		return nil, false
	}

	delete(session.Values, stateKeyName)

	return session, true
}

// stateBytes sized to 32 (256 bits) per OWASP modern-best-practice for
// OAuth state tokens. Previously 16 bytes, which is still cryptographically
// sufficient but below the recommended floor.
const stateBytes = 32

func generateState() (string, error) {
	buf := make([]byte, stateBytes)

	_, err := rand.Read(buf)
	if err != nil {
		return "", err //nolint:wrapcheck // internal helper, caller wraps
	}

	return hex.EncodeToString(buf), nil
}
