package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/lexfrei/cozytempl/internal/view/authpage"
)

// tokenFormField is the urlencoded form field the paste form uses.
// The HTML in token_upload.templ must match.
const tokenFormField = "token"

// tokenMaxBytes bounds the accepted Bearer token size.
//
// The hard ceiling is gorilla/securecookie's default maxLength of
// 4096 bytes for the *encoded* cookie value. Encoding a session that
// carries a raw N-byte token produces roughly:
//
//	gob(session)       ≈ N + 250   (map + 5 other keys + gob headers)
//	+ IV               + 16        (AES-CFB does not pad but prepends IV)
//	= cipher           ≈ N + 266
//	base64(cipher)     ≈ (N + 266) * 4/3
//	+ "name|date|"     + 21
//	+ HMAC             + 32
//	final base64       ≈ (base64(cipher) + 53) * 4/3
//
// Substituting N = 1500 lands at ~3900 encoded bytes — comfortably
// under both gorilla's 4096 default and RFC 6265's 4096-per-cookie
// guidance for browsers. Anything materially larger starts tripping
// securecookie's errEncodedValueTooLong, which store.Save surfaces
// as a generic session-save error (a 500 for the user with no hint
// what went wrong).
//
// Real-world Kubernetes service-account tokens are ~900-1500 bytes,
// so this cap is comfortable for every common case. Operators whose
// IdP mints tokens larger than 1.5 KB should use byok (kubeconfig
// mode) instead. CookieStore does NOT split a single session across
// multiple cookies, so raising this cap without also calling
// MaxLength on the underlying securecookie codecs would produce a
// 500 on first paste of an oversized token.
const tokenMaxBytes int64 = 1500

// ErrTokenEmpty is returned when the upload form arrives with no
// non-whitespace content in the token field.
var ErrTokenEmpty = errors.New("paste a Kubernetes Bearer token")

// ErrTokenTooLarge is returned when the trimmed token exceeds
// tokenMaxBytes. Surfaced verbatim so the user knows to check whether
// they pasted a kubeconfig or a multi-line blob by mistake.
var ErrTokenTooLarge = errors.New(
	"token is larger than 1.5 KB; paste a single Bearer token rather than a kubeconfig blob",
)

// ErrTokenUnreachable is returned when the pasted token parses as a
// non-empty string but the test SelfSubjectAccessReview call against
// the in-cluster apiserver fails — usually because the token is
// invalid, expired, or the apiserver rejected it.
var ErrTokenUnreachable = errors.New("apiserver rejected the token")

// ErrTokenProbeMisconfigured is returned when the probe has no
// base rest.Config to build its client from. Indicates a wiring
// error in main.go rather than a user-actionable condition.
var ErrTokenProbeMisconfigured = errors.New(
	"probe base config missing; wire k8sCfg into auth.NewHandler",
)

// probeTokenFn is the package-level seam that probeToken uses to talk
// to the apiserver. Tests overwrite it with a stub so the upload
// handler can be exercised without a live cluster.
//
//nolint:gochecknoglobals // intentional test seam
var probeTokenFn = probeToken

// HandleTokenUploadForm renders the empty paste form on GET. The form
// posts urlencoded data back to the same path.
func (hnd *Handler) HandleTokenUploadForm(writer http.ResponseWriter, req *http.Request) {
	renderTokenForm(writer, req, "")
}

// HandleTokenUpload handles the POST of a pasted Bearer token. On
// success the token is probed against the apiserver, stored in the
// encrypted session cookie, and the user is redirected to the
// dashboard. On failure the form is re-rendered with an inline error.
func (hnd *Handler) HandleTokenUpload(writer http.ResponseWriter, req *http.Request) {
	// Cap the request body BEFORE form parsing so a hostile client
	// cannot stream gigabytes at the parser. The 2× allowance
	// matches the BYOK handler.
	req.Body = http.MaxBytesReader(writer, req.Body, tokenMaxBytes*2)

	parseErr := req.ParseForm()
	if parseErr != nil {
		hnd.log.Warn("token upload: form parse failed", "error", parseErr)
		renderTokenForm(writer, req, "Invalid upload: "+parseErr.Error())

		return
	}

	token := strings.TrimSpace(req.FormValue(tokenFormField))

	if token == "" {
		renderTokenForm(writer, req, ErrTokenEmpty.Error())

		return
	}

	if int64(len(token)) > tokenMaxBytes {
		renderTokenForm(writer, req, ErrTokenTooLarge.Error())

		return
	}

	probeErr := probeTokenFn(req.Context(), hnd.baseCfg, token)
	if probeErr != nil {
		hnd.log.Info("token upload rejected", "error", probeErr)
		renderTokenForm(writer, req, userFacingProbeError(probeErr))

		return
	}

	hnd.persistTokenSession(writer, req, token)
}

// persistTokenSession writes the validated token into the session
// cookie and redirects to the dashboard. Extracted so HandleTokenUpload
// stays under the funlen budget.
func (hnd *Handler) persistTokenSession(writer http.ResponseWriter, req *http.Request, token string) {
	session, err := hnd.store.Get(req)
	if err != nil {
		hnd.log.Error("getting session for token upload", "error", err)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	SetBearerToken(session, token)
	SetUser(session, &UserSession{Username: usernameTokenMode})

	saveErr := hnd.store.Save(req, writer, session)
	if saveErr != nil {
		hnd.log.Error("saving session after token upload", "error", saveErr)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	hnd.log.Info("token uploaded", "bytes", len(token))
	http.Redirect(writer, req, "/", http.StatusSeeOther)
}

// userFacingProbeError strips upstream detail from probe failures
// that wrap ErrTokenUnreachable. Raw client-go network errors
// embed the apiserver address (e.g. 'dial tcp 10.96.0.1:443: i/o
// timeout') which leaks cluster topology to anyone who can reach
// the paste form. Full error detail still goes to the server log.
// Misconfiguration errors (ErrTokenProbeMisconfigured) describe
// our own wiring bug rather than a cluster address, so we surface
// them unchanged.
func userFacingProbeError(err error) string {
	if errors.Is(err, ErrTokenUnreachable) {
		return ErrTokenUnreachable.Error()
	}

	return err.Error()
}

// probeToken runs one cheap SelfSubjectAccessReview against the
// apiserver described by baseCfg, using the pasted token as the
// Bearer credential. The check itself doesn't matter — the
// round-trip is the signal we care about. baseCfg comes from
// main.go's loadKubeConfig() so the probe targets the same apiserver
// the rest of cozytempl talks to (in-cluster or $KUBECONFIG,
// whichever loadKubeConfig picked).
func probeToken(ctx context.Context, baseCfg *rest.Config, token string) error {
	if baseCfg == nil {
		return ErrTokenProbeMisconfigured
	}

	cfg := rest.CopyConfig(baseCfg)
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		// Wrap under ErrTokenUnreachable so userFacingProbeError
		// sanitises the detail. NewForConfig errors typically
		// surface when baseCfg.Host is malformed and the raw error
		// would echo that host to the browser.
		return fmt.Errorf("%w: %v", ErrTokenUnreachable, err) //nolint:errorlint // deliberate %v, see probeToken wrap note
	}

	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Verb:     "list",
				Group:    "helm.toolkit.fluxcd.io",
				Resource: "helmreleases",
			},
		},
	}

	_, err = client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		// %v keeps the upstream error out of the error chain
		// (errors.Is callers match the sentinel, not the transient
		// network detail) while preserving the text for the server
		// log. The user-facing path strips this detail anyway — see
		// userFacingProbeError — so the leak risk is bounded even if
		// someone flips to errors.Unwrap later.
		return fmt.Errorf("%w: %v", ErrTokenUnreachable, err) //nolint:errorlint // see comment above: deliberate %v
	}

	return nil
}

// renderTokenForm writes the upload form with an optional inline
// error. Used for both GET (empty) and failed POST retries.
func renderTokenForm(writer http.ResponseWriter, req *http.Request, errorMessage string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store, private")

	err := authpage.TokenUpload(errorMessage).Render(req.Context(), writer)
	if err != nil {
		slog.Default().Error("rendering token upload form", "error", err)
	}
}
