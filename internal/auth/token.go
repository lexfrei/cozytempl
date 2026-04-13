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

// tokenMaxBytes bounds the accepted Bearer token size. Real-world
// Kubernetes service-account tokens are well under 2 KB; 4 KB leaves
// headroom without bloating the cookie. The gorilla/sessions cookie
// store splits payloads larger than 4 KB across multiple cookies, so
// a 4 KB token still fits one chunk after gob+encrypt+base64 inflation
// only if the rest of the session is small — which it is in token mode
// (no OIDC tokens, no kubeconfig).
const tokenMaxBytes int64 = 4 * 1024

// ErrTokenEmpty is returned when the upload form arrives with no
// non-whitespace content in the token field.
var ErrTokenEmpty = errors.New("paste a Kubernetes Bearer token")

// ErrTokenTooLarge is returned when the trimmed token exceeds
// tokenMaxBytes. Surfaced verbatim so the user knows to check whether
// they pasted a kubeconfig or a multi-line blob by mistake.
var ErrTokenTooLarge = errors.New(
	"token is larger than 4 KB; paste a single Bearer token rather than a kubeconfig blob",
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
		renderTokenForm(writer, req, probeErr.Error())

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
		return fmt.Errorf("building clientset for probe: %w", err)
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
		return fmt.Errorf("%w: %v", ErrTokenUnreachable, err) //nolint:errorlint // wrap upstream as detail
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
