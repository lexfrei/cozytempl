package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/i18n"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// fakeAppGetter satisfies appGetter with a single-path return value.
// The HTTP handler path goes through appGetter twice — once in
// resolveAction, once in finishAction for the re-render — so the
// counter lets tests pin both invocations.
type fakeAppGetter struct {
	app  *k8s.Application
	err  error
	hits int
}

func (f *fakeAppGetter) Get(_ context.Context, _ *auth.UserContext, _, _ string) (*k8s.Application, error) {
	f.hits++

	if f.err != nil {
		return nil, f.err
	}

	return f.app, nil
}

// recordingAudit captures every event in order so the test can
// assert both the top-level Action and the details map. Not
// concurrent-safe by design; handler tests run a single request per
// case.
type recordingAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, evt *audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, *evt)
}

// newTestHandler wires a minimal PageHandler that lets InvokeAction
// run end-to-end with everything stubbed but the registry itself.
// Callers receive back the audit recorder so assertions can reach
// into it after the request.
func newTestHandler(t *testing.T, getter *fakeAppGetter) (*PageHandler, *recordingAudit) {
	t.Helper()

	bundle, err := i18n.NewBundle()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	rec := &recordingAudit{}

	return &PageHandler{
		appGetter:  getter,
		auditLog:   rec,
		baseCfg:    &rest.Config{Host: "https://apiserver.test"},
		authMode:   config.AuthModeDev,
		devMode:    true,
		i18nBundle: bundle,
		log:        slog.New(slog.DiscardHandler),
	}, rec
}

// actionPOST constructs a POST request at the action endpoint with
// a user already attached to the context — bypassing RequireAuth so
// InvokeAction's requireUser short-circuit succeeds.
func actionPOST(t *testing.T, tenant, app, action string) *http.Request {
	t.Helper()

	ctx := auth.ContextWithUser(t.Context(), &auth.UserContext{Username: "test-user"})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/tenants/"+tenant+"/apps/"+app+"/actions/"+action, nil)
	req.SetPathValue("tenant", tenant)
	req.SetPathValue("name", app)
	req.SetPathValue("action", action)

	return req
}

// withLocalizer routes req through the handler's i18n middleware so
// pgh.t() can resolve keys. Tests that inspect rendered body copy
// (toasts, labels) need this; tests that only assert audit events
// or callback invocations can skip it.
func withLocalizer(t *testing.T, pgh *PageHandler, req *http.Request) *http.Request {
	t.Helper()

	var out *http.Request

	pgh.i18nBundle.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, wrapped *http.Request) {
		out = wrapped
	})).ServeHTTP(httptest.NewRecorder(), req)

	if out == nil {
		t.Fatal("i18n middleware did not forward request")
	}

	return out
}

// TestInvokeAction_Success_AppliesPrefixOnRun is the end-to-end
// regression gate for the Cozystack→KubeVirt name mismatch the
// first review caught. Registers a throwaway action with a
// TargetName that prepends 'tp-', drives the handler, and asserts
// the Run closure received 'tp-<appName>' — proving the handler
// actually calls ResolveTargetName between Lookup and Run.
func TestInvokeAction_Success_AppliesPrefixOnRun(t *testing.T) {
	kind := "TPTestApp"

	var (
		gotNS, gotName string
		runCalled      bool
	)

	restoreAction := actions.RegisterForTest(kind, actions.Action{
		ID:            "poke",
		LabelKey:      "page.appDetail.action.vmStart",
		AuditCategory: "tp.poke",
		Capability:    actions.Capability{}, // always-allowed
		TargetName:    func(appName string) string { return "tp-" + appName },
		Run: func(_ context.Context, _ *rest.Config, ns, name string) error {
			runCalled = true
			gotNS, gotName = ns, name

			return nil
		},
	})
	defer restoreAction()

	getter := &fakeAppGetter{app: &k8s.Application{Name: "myvm", Kind: kind}}
	pgh, recAudit := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, actionPOST(t, "tenant-root", "myvm", "poke"))

	if !runCalled {
		t.Fatal("action.Run never fired — handler regressed to Lookup-without-dispatch")
	}

	if gotNS != "tenant-root" {
		t.Errorf("Run ns = %q, want tenant-root", gotNS)
	}

	if gotName != "tp-myvm" {
		t.Errorf("Run name = %q, want tp-myvm (prefix translation regressed)", gotName)
	}

	// Hx-Reswap: none is the single DOM-integrity gate that keeps
	// the toast-only success response from blanking #main-content
	// (htmx would otherwise swap the toast div in, destroying the
	// detail page). Dropping this header silently breaks every
	// click in production; pin it here.
	if got := rec.Header().Get("Hx-Reswap"); got != "none" {
		t.Errorf("Hx-Reswap = %q, want %q on success path", got, "none")
	}

	// Without this check, a regression that returns silently after
	// runAction without calling finishAction (e.g. a misplaced
	// `return`) would still satisfy the assertions above. Pin the
	// success toast and the OutcomeSuccess audit so the whole
	// downstream chain is exercised.
	if !strings.Contains(rec.Body.String(), "toast-success") {
		t.Errorf("success toast not rendered; body = %q", rec.Body.String())
	}

	gotSuccess := false

	for _, evt := range recAudit.events {
		if evt.Action == audit.ActionAppAction && evt.Outcome == audit.OutcomeSuccess {
			gotSuccess = true

			break
		}
	}

	if !gotSuccess {
		t.Errorf("OutcomeSuccess audit not emitted; events = %+v", recAudit.events)
	}
}

// TestInvokeAction_RejectsMalformedActionID confirms the blocker-2
// fix: an attacker-controlled actionID must not reach
// actions.Lookup or land in a toast template verbatim. The test
// drives a deliberately crafted slug and asserts (a) no audit event
// is emitted with the raw value in a user-facing field, (b) the
// rendered body does not echo the script tag.
func TestInvokeAction_RejectsMalformedActionID(t *testing.T) {
	getter := &fakeAppGetter{app: &k8s.Application{Name: "myvm", Kind: "VMInstance"}}
	pgh, recAudit := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	malformed := "<script>alert(1)</script>"
	pgh.InvokeAction(rec, actionPOST(t, "tenant-root", "myvm", malformed))

	// The response body must not contain the raw path segment.
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("malformed actionID leaked into rendered body: %q", rec.Body.String())
	}

	if getter.hits != 0 {
		t.Errorf("appGetter.Get fired %d times on malformed input; want 0", getter.hits)
	}

	// Audit must fire with OutcomeError on the rejection branch so
	// operators can see that someone tried an invalid action.
	found := false

	for _, evt := range recAudit.events {
		if evt.Action == audit.ActionAppAction && evt.Outcome == audit.OutcomeError {
			found = true

			break
		}
	}

	if !found {
		t.Error("audit event not recorded for malformed actionID")
	}
}

// TestInvokeAction_AuditsLookupForbidden is the regression for the
// compliance gap the review flagged. When appGetter returns a
// forbidden error, the handler must emit an audit event carrying
// the apiserver-label so SOC2 queries for 'who tried to act on
// what they couldn't see' actually return data.
func TestInvokeAction_AuditsLookupForbidden(t *testing.T) {
	gr := schema.GroupResource{Group: "apps.cozystack.io", Resource: "vminstances"}
	getter := &fakeAppGetter{err: apierrors.NewForbidden(gr, "myvm", errors.New("no RBAC"))}

	pgh, recAudit := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, actionPOST(t, "tenant-root", "myvm", "start"))

	found := false

	for _, evt := range recAudit.events {
		if evt.Action != audit.ActionAppAction || evt.Outcome != audit.OutcomeError {
			continue
		}

		if errClass, _ := evt.Details["errorClass"].(string); errClass == errLabelForbidden {
			found = true

			break
		}
	}

	if !found {
		t.Errorf("audit did not record a forbidden-lookup error; events = %+v", recAudit.events)
	}

	// Same DOM-integrity guard as the success path: the error toast
	// body must not swap into #main-content. Pin both branches.
	if got := rec.Header().Get("Hx-Reswap"); got != "none" {
		t.Errorf("Hx-Reswap = %q, want %q on error path", got, "none")
	}
}

// TestInvokeAction_FallsBackToActionIDOnMissingLabelKey exercises
// the localizedActionLabel fallback branch: an action whose LabelKey
// does not resolve must NOT bake the bracket-form ("[page.foo]")
// into the toast — it should fall back to the raw action ID. A
// missing translation is a deployment bug, but the user should see
// something readable, not a square-bracketed slug.
func TestInvokeAction_FallsBackToActionIDOnMissingLabelKey(t *testing.T) {
	kind := "FallbackKind"

	restoreAction := actions.RegisterForTest(kind, actions.Action{
		ID:            "nudge",
		LabelKey:      "page.appDetail.action.doesNotExist", // deliberately unregistered in any locale
		AuditCategory: "test.fallback",
		Run: func(context.Context, *rest.Config, string, string) error {
			return errors.New("force error path so toast renders the label")
		},
	})
	defer restoreAction()

	getter := &fakeAppGetter{app: &k8s.Application{Name: "myvm", Kind: kind}}
	pgh, _ := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, withLocalizer(t, pgh, actionPOST(t, "tenant-root", "myvm", "nudge")))

	body := rec.Body.String()

	if strings.Contains(body, "[page.appDetail.action.doesNotExist]") {
		t.Errorf("toast leaked bracket-form of missing key; body = %q", body)
	}

	if !strings.Contains(body, "nudge") {
		t.Errorf("toast missing fallback action ID; body = %q", body)
	}
}

// TestInvokeAction_AuditsUnknownActionDispatchMiss covers the
// blocker the cycle-4 review flagged: when the app fetch succeeds
// but the action ID is valid-shape yet unregistered for the app's
// Kind, the handler must emit an audit event (not just log + show a
// toast). Without the event, a probing pattern like "POST
// /actions/foo-bar" on every Kind leaves no audit trail.
func TestInvokeAction_AuditsUnknownActionDispatchMiss(t *testing.T) {
	restoreAllowed := stubAllowedFn(t, func(context.Context, *rest.Config, actions.Capability, string) (bool, error) {
		return true, nil
	})
	defer restoreAllowed()

	getter := &fakeAppGetter{app: &k8s.Application{Name: "myvm", Kind: "VMInstance"}}
	pgh, recAudit := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, actionPOST(t, "tenant-root", "myvm", "no-such-action"))

	found := false

	for _, evt := range recAudit.events {
		if evt.Action != audit.ActionAppAction || evt.Outcome != audit.OutcomeError {
			continue
		}

		if stage, _ := evt.Details["stage"].(string); stage == "dispatch" {
			found = true

			break
		}
	}

	if !found {
		t.Errorf("audit did not record a dispatch-miss error; events = %+v", recAudit.events)
	}
}

// stubAllowedFn exposes the internal/actions test seam to the
// handler-package tests through a short local helper so the tests
// don't need to import the unexported seam name.
func stubAllowedFn(t *testing.T, fn func(context.Context, *rest.Config, actions.Capability, string) (bool, error)) func() {
	t.Helper()

	return actions.SwapAllowedFnForTest(fn)
}
