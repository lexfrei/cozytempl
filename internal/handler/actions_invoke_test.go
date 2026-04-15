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
// Callers receive back the audit recorder and the fake getter so
// assertions can reach into them after the request.
func newTestHandler(t *testing.T, getter *fakeAppGetter) (*PageHandler, *recordingAudit, *i18n.Bundle) {
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
	}, rec, bundle
}

// actionPOST constructs a POST request at the action endpoint with
// a user already attached to the context — bypassing RequireAuth so
// InvokeAction's requireUser short-circuit succeeds.
func actionPOST(t *testing.T, bundle *i18n.Bundle, tenant, app, action string) *http.Request {
	t.Helper()

	_ = bundle // kept in the signature so future tests can attach
	// a specific Localizer to the context; today pgh.t() falls back
	// to the English Localizer via i18n.LocalizerFromContext when
	// the context has none, which is fine for handler-level tests.

	ctx := auth.ContextWithUser(t.Context(), &auth.UserContext{Username: "test-user"})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/tenants/"+tenant+"/apps/"+app+"/actions/"+action, nil)
	req.SetPathValue("tenant", tenant)
	req.SetPathValue("name", app)
	req.SetPathValue("action", action)

	return req
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

	// Stub the capability probe — the filter is skipped when
	// resolveAction is the only app fetch, but the re-render path
	// in finishAction calls capabilityProbedActions which probes
	// every registered action. Return allow.
	restoreAllowed := stubAllowedFn(t, func(context.Context, *rest.Config, actions.Capability, string) (bool, error) {
		return true, nil
	})
	defer restoreAllowed()

	actions.Register(kind, actions.Action{
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

	getter := &fakeAppGetter{app: &k8s.Application{Name: "myvm", Kind: kind}}
	pgh, _, bundle := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, actionPOST(t, bundle, "tenant-root", "myvm", "poke"))

	if !runCalled {
		t.Fatal("action.Run never fired — handler regressed to Lookup-without-dispatch")
	}

	if gotNS != "tenant-root" {
		t.Errorf("Run ns = %q, want tenant-root", gotNS)
	}

	if gotName != "tp-myvm" {
		t.Errorf("Run name = %q, want tp-myvm (prefix translation regressed)", gotName)
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
	pgh, recAudit, bundle := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	malformed := "<script>alert(1)</script>"
	pgh.InvokeAction(rec, actionPOST(t, bundle, "tenant-root", "myvm", malformed))

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

	pgh, recAudit, bundle := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, actionPOST(t, bundle, "tenant-root", "myvm", "start"))

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
	pgh, recAudit, bundle := newTestHandler(t, getter)

	rec := httptest.NewRecorder()
	pgh.InvokeAction(rec, actionPOST(t, bundle, "tenant-root", "myvm", "no-such-action"))

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
