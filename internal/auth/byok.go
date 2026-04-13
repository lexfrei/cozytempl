package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/lexfrei/cozytempl/internal/view/authpage"
)

// kubeconfigFormField is the multipart field name the upload form
// uses. The HTML in kubeconfig_upload.templ must match.
const kubeconfigFormField = "kubeconfig"

// kubeconfigMaxBytes bounds the accepted file size. Gorilla cookie
// sessions encode bytes → gob → encrypt → base64, which inflates
// roughly 3×. A 32 KB source produces ~100 KB cookie payload,
// which is beyond every browser's 4 KB per-cookie limit but still
// compatible with sessions split across multiple cookies (which
// gorilla does transparently). Past 32 KB we risk exceeding total
// per-domain cookie limits too.
const kubeconfigMaxBytes int64 = 32 * 1024

// ErrKubeconfigTooLarge is returned when an uploaded file exceeds
// kubeconfigMaxBytes. Surfaced to the user verbatim so they know to
// trim their kubeconfig.
var ErrKubeconfigTooLarge = errors.New(
	"kubeconfig is larger than 32 KB; strip unused contexts with `kubectl config view --minify --flatten`",
)

// ErrKubeconfigExecPlugin is returned when the uploaded kubeconfig's
// current-context user is configured with an exec or auth-provider
// plugin — those require an interactive shell the cozytempl pod
// cannot provide.
var ErrKubeconfigExecPlugin = errors.New(
	"exec plugins are not supported; generate a static token with `kubectl create token` and reference it directly",
)

// ErrKubeconfigNoContext is returned when the uploaded kubeconfig
// has no current-context or the current-context does not resolve
// to a valid cluster+user pair.
var ErrKubeconfigNoContext = errors.New("kubeconfig has no usable current-context")

// ErrKubeconfigUnreachable is returned when the uploaded
// kubeconfig parses cleanly but the test SelfSubjectAccessReview
// call fails — usually because the cluster URL is not reachable
// from the cozytempl pod network.
var ErrKubeconfigUnreachable = errors.New("cluster is not reachable from the cozytempl pod")

// HandleKubeconfigUploadForm renders the empty upload form on GET.
// The form posts multipart/form-data back to the same path.
func (hnd *Handler) HandleKubeconfigUploadForm(writer http.ResponseWriter, req *http.Request) {
	renderKubeconfigForm(writer, req, "")
}

// HandleKubeconfigUpload handles the POST of a kubeconfig file. On
// success the file is validated, stored in the encrypted session
// cookie, and the user is redirected to the dashboard. On failure
// the upload form is re-rendered with an inline error message.
func (hnd *Handler) HandleKubeconfigUpload(writer http.ResponseWriter, req *http.Request) {
	// Cap the request body BEFORE multipart parsing so a hostile
	// client cannot stream gigabytes at the parser.
	req.Body = http.MaxBytesReader(writer, req.Body, kubeconfigMaxBytes*2)

	parseErr := req.ParseMultipartForm(kubeconfigMaxBytes)
	if parseErr != nil {
		hnd.log.Warn("kubeconfig upload: multipart parse failed", "error", parseErr)
		renderKubeconfigForm(writer, req, "Invalid upload: "+parseErr.Error())

		return
	}

	bytes, readErr := readKubeconfigUpload(req)
	if readErr != nil {
		renderKubeconfigForm(writer, req, readErr.Error())

		return
	}

	validateErr := validateKubeconfigBytes(req.Context(), bytes)
	if validateErr != nil {
		hnd.log.Info("kubeconfig upload rejected", "error", validateErr)
		// ErrKubeconfigUnreachable wraps a client-go network error
		// whose message embeds the apiserver host and port — sanitize
		// to the generic sentinel so the paste form never leaks
		// internal cluster topology. Parse / exec-plugin errors
		// describe the user's own kubeconfig, so they pass through.
		renderKubeconfigForm(writer, req, sanitizeKubeconfigError(validateErr))

		return
	}

	session, err := hnd.store.Get(req)
	if err != nil {
		hnd.log.Error("getting session for kubeconfig upload", "error", err)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	SetKubeconfig(session, bytes)
	SetUser(session, &UserSession{Username: "kubeconfig-user"})

	saveErr := hnd.store.Save(req, writer, session)
	if saveErr != nil {
		hnd.log.Error("saving session after kubeconfig upload", "error", saveErr)
		http.Error(writer, `{"error":"session error"}`, http.StatusInternalServerError)

		return
	}

	hnd.log.Info("kubeconfig uploaded", "bytes", len(bytes))
	http.Redirect(writer, req, "/", http.StatusSeeOther)
}

// sanitizeKubeconfigError hides the wrapped client-go network detail
// from probe failures that reach the user-facing form. Non-probe
// errors (parse failures, exec-plugin rejections, empty-context
// errors) describe the user's own input and pass through unchanged.
func sanitizeKubeconfigError(err error) string {
	if errors.Is(err, ErrKubeconfigUnreachable) {
		return ErrKubeconfigUnreachable.Error()
	}

	return err.Error()
}

// readKubeconfigUpload pulls the raw bytes out of the multipart
// form. Enforces the size cap a second time in case the multipart
// parser lets slightly more through.
func readKubeconfigUpload(req *http.Request) ([]byte, error) {
	file, _, err := req.FormFile(kubeconfigFormField)
	if err != nil {
		return nil, fmt.Errorf("no kubeconfig file in form: %w", err)
	}

	defer func() { _ = file.Close() }()

	limited := io.LimitReader(file, kubeconfigMaxBytes+1)

	bytes, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading uploaded file: %w", err)
	}

	if int64(len(bytes)) > kubeconfigMaxBytes {
		return nil, ErrKubeconfigTooLarge
	}

	return bytes, nil
}

// validateKubeconfigBytes parses and sanity-checks the uploaded
// kubeconfig. Anything that trips up clientcmd, references an
// exec plugin, or fails a SelfSubjectAccessReview test call comes
// back as an error the caller surfaces to the user.
func validateKubeconfigBytes(ctx context.Context, raw []byte) error {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return fmt.Errorf("parsing kubeconfig: %w", err)
	}

	rejectErr := rejectExecPluginUsers(cfg)
	if rejectErr != nil {
		return rejectErr
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return fmt.Errorf("building rest config: %w", err)
	}

	return probeKubeconfig(ctx, restCfg)
}

// rejectExecPluginUsers returns an error if the kubeconfig's
// current-context user (or any AuthInfo it references) is
// configured with an exec or auth-provider plugin.
func rejectExecPluginUsers(cfg *clientcmdapi.Config) error {
	contextName := cfg.CurrentContext
	if contextName == "" {
		return ErrKubeconfigNoContext
	}

	kubeCtx, ok := cfg.Contexts[contextName]
	if !ok {
		return ErrKubeconfigNoContext
	}

	authInfo, ok := cfg.AuthInfos[kubeCtx.AuthInfo]
	if !ok {
		return ErrKubeconfigNoContext
	}

	if authInfo.Exec != nil || authInfo.AuthProvider != nil {
		return ErrKubeconfigExecPlugin
	}

	return nil
}

// probeKubeconfig does one cheap SelfSubjectAccessReview to confirm
// the cluster is reachable and the credential is accepted. The
// resource is intentionally fake — the API server answers without
// needing any specific RBAC, and the round-trip itself is the
// signal we care about.
func probeKubeconfig(ctx context.Context, restCfg *rest.Config) error {
	client, err := kubernetes.NewForConfig(restCfg)
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
		return fmt.Errorf("%w: %v", ErrKubeconfigUnreachable, err) //nolint:errorlint // wrap upstream as detail
	}

	return nil
}

// renderKubeconfigForm writes the upload form with an optional
// inline error. Used for both GET (empty) and failed POST retries.
func renderKubeconfigForm(writer http.ResponseWriter, req *http.Request, errorMessage string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store, private")

	err := authpage.KubeconfigUpload(errorMessage).Render(req.Context(), writer)
	if err != nil {
		slog.Default().Error("rendering kubeconfig upload form", "error", err)
	}
}
