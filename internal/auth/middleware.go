package auth

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/sessions"
	"github.com/lexfrei/cozytempl/internal/config"
)

type contextKey string

const (
	userContextKey     contextKey = "user"
	authModeContextKey contextKey = "auth_mode"
)

// refreshLeadTime is the slack before ID token expiry at which the
// middleware proactively calls Refresh. A request that arrives within
// this window triggers a single round-trip to Keycloak before the k8s
// API call. Picked to be longer than the longest realistic k8s request
// path so the token will still be valid when it actually reaches the
// apiserver, but short enough that we don't pressure the OIDC provider.
const refreshLeadTime = 60 * time.Second

// usernameTokenMode is the synthetic username assigned to sessions in
// AuthModeToken — the actual identity is the pasted Bearer token, but
// the UI still wants a string to display.
const usernameTokenMode = "token-user"

// RequireAuth is middleware that checks for a valid session and, in
// passthrough mode, proactively refreshes the ID token before it
// expires. In byok mode, a missing kubeconfig redirects the user to
// the upload form; in token mode, a missing Bearer token redirects
// to the paste form. In all authenticated modes the resolved
// UserContext is attached to the request context for downstream
// handlers.
//
// The oidc argument may be nil in modes that do not use OIDC (byok,
// token, dev) — the middleware only dereferences it when running in
// a mode that needs token refresh.
func RequireAuth(
	store *SessionStore,
	oidc *OIDCProvider,
	log *slog.Logger,
	mode config.AuthMode,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		session, err := store.Get(req)
		if err != nil {
			http.Error(writer, `{"error":"invalid session"}`, http.StatusUnauthorized)

			return
		}

		info := GetUser(session)

		// BYOK has a different auth check: the user is authenticated
		// once they have uploaded a kubeconfig. There is no OIDC
		// session at all.
		if mode == config.AuthModeBYOK {
			serveBYOK(writer, req, session, &info, next)

			return
		}

		// Token mode: same shape as BYOK but the credential is a
		// pasted Bearer token rather than a kubeconfig file.
		if mode == config.AuthModeToken {
			serveToken(writer, req, session, &info, next)

			return
		}

		serveOIDC(writer, req, session, store, oidc, log, mode, &info, next)
	})
}

// serveOIDC handles the passthrough / impersonation-legacy request flow.
// Extracted from RequireAuth so the latter stays under the funlen limit
// once additional auth modes are dispatched at the top.
func serveOIDC(
	writer http.ResponseWriter,
	req *http.Request,
	session *sessions.Session,
	store *SessionStore,
	oidc *OIDCProvider,
	log *slog.Logger,
	mode config.AuthMode,
	info *UserSession,
	next http.Handler,
) {
	if info.Username == "" {
		http.Error(writer, `{"error":"not authenticated"}`, http.StatusUnauthorized)

		return
	}

	idToken := info.IDToken

	if mode == config.AuthModePassthrough {
		refreshed, refreshErr := refreshIfNeeded(req.Context(), session, store, oidc, log, writer, req, info)
		if refreshErr != nil {
			// The helper already took care of redirect/401 so
			// the downstream handler must NOT run.
			return
		}

		idToken = refreshed
	}

	usr := &UserContext{
		Username: info.Username,
		Groups:   info.Groups,
		IDToken:  idToken,
	}

	ctx := ContextWithUser(req.Context(), usr)
	ctx = ContextWithAuthMode(ctx, mode)
	next.ServeHTTP(writer, req.WithContext(ctx))
}

// serveBYOK wires the BYOK-mode request flow. A user with no stored
// kubeconfig is bounced to the upload form; a user who already has
// one proceeds to the handler with KubeconfigBytes populated.
func serveBYOK(
	writer http.ResponseWriter,
	req *http.Request,
	session *sessions.Session,
	info *UserSession,
	next http.Handler,
) {
	kubeconfig, ok := GetKubeconfig(session)
	if !ok {
		// Not an API path? Redirect to upload form. API calls return
		// 401 so htmx surfaces an error instead of swapping HTML.
		if isAPIRequest(req) {
			http.Error(writer, `{"error":"kubeconfig not uploaded"}`, http.StatusUnauthorized)

			return
		}

		http.Redirect(writer, req, pathAuthKubeconfig, http.StatusFound)

		return
	}

	username := info.Username
	if username == "" {
		username = "kubeconfig-user"
	}

	usr := &UserContext{
		Username:        username,
		Groups:          info.Groups,
		KubeconfigBytes: kubeconfig,
	}

	ctx := ContextWithUser(req.Context(), usr)
	ctx = ContextWithAuthMode(ctx, config.AuthModeBYOK)
	next.ServeHTTP(writer, req.WithContext(ctx))
}

// serveToken wires the token-mode request flow. A user with no stored
// Bearer token is bounced to the paste form; a user who already has one
// proceeds to the handler with BearerToken populated.
func serveToken(
	writer http.ResponseWriter,
	req *http.Request,
	session *sessions.Session,
	info *UserSession,
	next http.Handler,
) {
	token, ok := GetBearerToken(session)
	if !ok {
		if isAPIRequest(req) {
			http.Error(writer, `{"error":"token not provided"}`, http.StatusUnauthorized)

			return
		}

		http.Redirect(writer, req, pathAuthToken, http.StatusFound)

		return
	}

	username := info.Username
	if username == "" {
		username = usernameTokenMode
	}

	usr := &UserContext{
		Username:    username,
		Groups:      info.Groups,
		BearerToken: token,
	}

	ctx := ContextWithUser(req.Context(), usr)
	ctx = ContextWithAuthMode(ctx, config.AuthModeToken)
	next.ServeHTTP(writer, req.WithContext(ctx))
}

// refreshIfNeeded looks at the stored ID token expiry and, if the token
// is close to expiring (or already expired), calls the OIDC provider's
// refresh endpoint. Success overwrites the session cookie with the new
// tokens and returns the new raw ID token; failure clears the session
// and redirects the user back to the login flow.
func refreshIfNeeded(
	ctx context.Context,
	session *sessions.Session,
	store *SessionStore,
	oidc *OIDCProvider,
	log *slog.Logger,
	writer http.ResponseWriter,
	req *http.Request,
	info *UserSession,
) (string, error) {
	if !needsRefresh(info) {
		return info.IDToken, nil
	}

	if oidc == nil || info.RefreshToken == "" {
		clearAndRedirect(session, store, log, writer, req)

		return "", errRefreshUnavailable
	}

	result, err := oidc.Refresh(ctx, info.RefreshToken)
	if err != nil {
		log.Warn("oidc refresh failed; clearing session", "error", err, "user", info.Username)
		clearAndRedirect(session, store, log, writer, req)

		return "", err
	}

	persistRefreshed(session, store, log, writer, req, info, result)

	return result.IDToken, nil
}

// needsRefresh decides whether the currently-held ID token is close
// enough to expiry (or already past it) that a refresh round-trip is
// warranted. Separated out so refreshIfNeeded stays readable and so
// tests can exercise the policy directly.
func needsRefresh(info *UserSession) bool {
	expiry := time.Unix(info.IDTokenExpiry, 0)
	if info.IDTokenExpiry == 0 {
		// Legacy session without the new field — try to parse the
		// token itself. If that fails too we treat the token as
		// expired so the refresh path runs.
		parsed, parseErr := IDTokenExpiry(info.IDToken)
		if parseErr == nil {
			expiry = parsed
		}
	}

	return time.Until(expiry) < refreshLeadTime
}

// persistRefreshed stores the newly-obtained tokens in the session and
// writes the updated cookie back to the response. Any store error is
// logged but not surfaced — the refresh itself succeeded, and the next
// request will just refresh again.
func persistRefreshed(
	session *sessions.Session,
	store *SessionStore,
	log *slog.Logger,
	writer http.ResponseWriter,
	req *http.Request,
	info *UserSession,
	result *ExchangeResult,
) {
	newExpiry, expErr := IDTokenExpiry(result.IDToken)
	if expErr != nil {
		log.Warn("refreshed id token has no parsable expiry", "error", expErr)
	}

	newRefresh := result.RefreshToken
	if newRefresh == "" {
		// Some providers return an empty refresh token when they
		// do not rotate — keep the old one so the session stays
		// refreshable.
		newRefresh = info.RefreshToken
	}

	SetUser(session, &UserSession{
		Username:      result.Claims.Username,
		Groups:        result.Claims.Groups,
		IDToken:       result.IDToken,
		RefreshToken:  newRefresh,
		IDTokenExpiry: newExpiry.Unix(),
	})

	saveErr := store.Save(req, writer, session)
	if saveErr != nil {
		log.Error("saving refreshed session", "error", saveErr)
	}
}

// errRefreshUnavailable is an internal sentinel used by refreshIfNeeded
// to tell the middleware "I already wrote a response, bail out." The
// value is never surfaced outside the package.
var errRefreshUnavailable = &refreshError{msg: "refresh unavailable"}

type refreshError struct{ msg string }

func (e *refreshError) Error() string { return e.msg }

// clearAndRedirect wipes the session and redirects the browser to the
// login page. API requests get a 401 JSON response instead so htmx can
// surface the error without swapping HTML.
func clearAndRedirect(
	session *sessions.Session,
	store *SessionStore,
	log *slog.Logger,
	writer http.ResponseWriter,
	req *http.Request,
) {
	Clear(session)

	err := store.Save(req, writer, session)
	if err != nil {
		log.Error("clearing session", "error", err)
	}

	if isAPIRequest(req) {
		http.Error(writer, `{"error":"session expired"}`, http.StatusUnauthorized)

		return
	}

	http.Redirect(writer, req, pathAuthLogin, http.StatusFound)
}

// isAPIRequest returns true for paths whose failure mode is a JSON 401
// rather than an HTML redirect. htmx consumes /api/* responses and
// surfaces 401s as form errors; a redirect would swap HTML into the
// target element, which is usually wrong.
func isAPIRequest(req *http.Request) bool {
	path := req.URL.Path

	return len(path) >= len("/api/") && path[:len("/api/")] == "/api/"
}

// UserContext holds the authenticated user's identity in request context.
//
// IDToken is populated in passthrough mode only; KubeconfigBytes in byok
// mode only; BearerToken in token mode only. In dev and
// impersonation-legacy modes all credential fields are empty and the
// NewUserClient factory dispatches on AuthMode rather than the presence
// of these fields.
type UserContext struct {
	Username        string
	Groups          []string
	IDToken         string
	KubeconfigBytes []byte
	BearerToken     string
}

// UserFromContext extracts the authenticated user from the request context.
func UserFromContext(ctx context.Context) *UserContext {
	usr, _ := ctx.Value(userContextKey).(*UserContext)

	return usr
}

// ContextWithUser attaches a UserContext to ctx using the same key
// the auth middleware uses. Exists so tests (and any caller that
// needs to simulate an authenticated request without going through
// RequireAuth / DevAuth) can build a request context without having
// to know the unexported key constant.
func ContextWithUser(ctx context.Context, usr *UserContext) context.Context {
	return context.WithValue(ctx, userContextKey, usr)
}

// ContextWithAuthMode attaches the active AuthMode to ctx so downstream
// code (audit log, metrics, UI templates) can react to the mode without
// threading a parameter through every function.
func ContextWithAuthMode(ctx context.Context, mode config.AuthMode) context.Context {
	return context.WithValue(ctx, authModeContextKey, mode)
}

// ModeFromContext returns the AuthMode attached by the middleware, or
// the empty string if none is present (e.g. in a test context that
// bypassed the middleware).
func ModeFromContext(ctx context.Context) config.AuthMode {
	mode, _ := ctx.Value(authModeContextKey).(config.AuthMode)

	return mode
}
