package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// Error-label constants fed into audit event details so log queries
// can group 'denied by RBAC' separately from 'target missing' without
// parsing error strings. New labels land here — never rename the
// shipped ones because operator alert rules key on them.
const (
	errLabelForbidden    = "forbidden"
	errLabelNotFound     = "notFound"
	errLabelUnauthorized = "unauthorized"
	errLabelOther        = "other"
)

// InvokeAction runs a per-resource action registered under the
// application's Kind. The path /tenants/{tenant}/apps/{name}/actions/{action}
// carries the tenant namespace, the Cozystack application name, and
// the action ID; the handler looks up the app, dispatches to the
// registered Run closure, and emits an audit event with the outcome.
//
// Errors from the action (apiserver 4xx / 5xx, credential problems,
// dispatch misses) land as a toast on the originating page. On success
// the user sees a "VM start queued" style toast and the status badge
// on the overview tab flips asynchronously when the reconciler catches
// up — this handler does NOT block waiting for the apiserver to mark
// the target ready, so a slow-responding subresource cannot stall the
// UI.
func (pgh *PageHandler) InvokeAction(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenantNS := req.PathValue("tenant")
	appName := req.PathValue("name")
	actionID := req.PathValue("action")

	if !k8s.IsValidLabelValue(tenantNS) || !k8s.IsValidLabelValue(appName) {
		pgh.log.Warn("action invoked with malformed path value",
			"tenant", tenantNS, "name", appName, "action", actionID)
		pgh.renderErrorToast(writer, req,
			pgh.t(req, "error.app.actionFailed", map[string]any{"Action": actionID}))

		return
	}

	action, kind, ok := pgh.resolveAction(req, usr, tenantNS, appName, actionID, writer)
	if !ok {
		return
	}

	runErr := pgh.runAction(req.Context(), usr, action, tenantNS, appName)
	pgh.finishAction(writer, req, usr, action, kind, tenantNS, appName, actionID, runErr)
}

// resolveAction looks up the Cozystack application and finds the
// registered action for its Kind. On any miss it writes the
// appropriate error toast directly and returns ok=false — callers
// MUST bail out of the request immediately in that case.
func (pgh *PageHandler) resolveAction(
	req *http.Request, usr *auth.UserContext,
	tenantNS, appName, actionID string,
	writer http.ResponseWriter,
) (actions.Action, string, bool) {
	app, err := pgh.appSvc.Get(req.Context(), usr, tenantNS, appName)
	if err != nil {
		pgh.log.Error("getting app for action", "tenant", tenantNS, "name", appName, "error", err)

		switch apiserverErrorLabel(err) {
		case errLabelNotFound:
			pgh.renderErrorToast(writer, req,
				pgh.t(req, "error.app.actionLookupNotFound", map[string]any{"Name": appName}))
		case errLabelForbidden, errLabelUnauthorized:
			pgh.renderErrorToast(writer, req,
				pgh.t(req, "error.app.actionLookupForbidden", map[string]any{"Name": appName}))
		default:
			pgh.renderErrorToast(writer, req,
				pgh.t(req, "error.app.actionLookup", map[string]any{"Name": appName}))
		}

		return actions.Action{}, "", false
	}

	action, found := actions.Lookup(app.Kind, actionID)
	if !found {
		pgh.log.Warn("unknown action requested",
			"tenant", tenantNS, "name", appName, "kind", app.Kind, "action", actionID)
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.actionUnknown", map[string]any{"Action": actionID}))

		return actions.Action{}, "", false
	}

	return action, app.Kind, true
}

// runAction builds the user-credentialed rest.Config and invokes the
// registered Run closure. Returns nil on success or the Run error
// verbatim; the caller decides how to surface it (audit + toast).
//
//nolint:gocritic // hugeParam: one copy per HTTP request, well off the hot path
func (pgh *PageHandler) runAction(
	ctx context.Context, usr *auth.UserContext,
	action actions.Action, tenantNS, appName string,
) error {
	userCfg, err := k8s.BuildUserRESTConfig(pgh.baseCfg, usr, pgh.authMode)
	if err != nil {
		return fmt.Errorf("building user rest config for action: %w", err)
	}

	// The Cozystack application name ≠ the KubeVirt VM name.
	// ResolveTargetName applies whatever prefix transformation the
	// Action carries (vm-instance- for VMInstance) before we hit the
	// subresource endpoint; without this the PUT 404s in production
	// even though the SSAR probe approved the click.
	targetName := action.ResolveTargetName(appName)

	runErr := action.Run(ctx, userCfg, tenantNS, targetName)
	if runErr != nil {
		return fmt.Errorf("running %s action: %w", action.ID, runErr)
	}

	return nil
}

// finishAction writes the audit event and the user-visible response
// for both the success and error paths.
//
// On error: an OOB toast (with Hx-Reswap: none so the triggering
// button stays put) — the detail page doesn't need to re-render
// because nothing on the apiserver changed.
//
// On success: the whole detail page is re-rendered into #main-content
// so the status badge, conditions tab, and events tab all reflect the
// new state the action just triggered. A success toast rides along
// OOB. The re-render is why the buttons hx-target #main-content —
// the toast docstring mentions 'queued' but the user actually gets
// fresh page state on the same response, not just a confirmation.
//
//nolint:gocritic // hugeParam: one copy per HTTP request, well off the hot path
func (pgh *PageHandler) finishAction(
	writer http.ResponseWriter, req *http.Request, usr *auth.UserContext,
	action actions.Action, kind, tenantNS, appName, actionID string, runErr error,
) {
	if runErr != nil {
		pgh.log.Warn("action failed",
			"tenant", tenantNS, "name", appName, "action", actionID, "error", runErr)
		pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
			audit.OutcomeError, map[string]any{
				"subaction": action.AuditCategory,
				"kind":      kind,
				"error":     runErr.Error(),
				"runtime":   apiserverErrorLabel(runErr),
			})
		pgh.renderErrorToast(writer, req,
			pgh.t(req, "error.app.actionFailed", map[string]any{"Action": actionID}))

		return
	}

	pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
		audit.OutcomeSuccess, map[string]any{
			"subaction": action.AuditCategory,
			"kind":      kind,
		})

	pgh.emitSuccessToast(writer, req,
		pgh.t(req, "toast.app.actionQueued", map[string]any{
			"Name":   appName,
			"Action": actionID,
		}))

	// Re-render the detail page so the status badge / conditions /
	// events reflect the state change the action kicked off. Passing
	// "overview" as the tab keeps the user on the page they already
	// saw — we don't want to kidnap them to a different tab.
	//
	// If the re-fetch fails (ctx cancelled, apiserver wobble, token
	// expired between calls), tell htmx NOT to swap the target DOM
	// so the user keeps the existing detail view with just the
	// success toast on top. Without Hx-Reswap here, htmx would swap
	// the response body — now containing only the OOB toast div —
	// into #main-content and blank the tenant detail page.
	app, err := pgh.appSvc.Get(req.Context(), usr, tenantNS, appName)
	if err != nil {
		pgh.log.Error("re-getting app after action", "tenant", tenantNS, "name", appName, "error", err)
		writer.Header().Set("Hx-Reswap", "none")

		return
	}

	data := pgh.buildAppDetailData(req, usr, tenantNS, appName, app, "overview")

	renderErr := page.AppDetail(data).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering detail after action", "error", renderErr)
	}
}

// apiserverErrorLabel gives the audit log a compact machine-readable
// tag for the failure family (forbidden / notFound / unauthorized /
// other) so log queries can group "denied by RBAC" separately from
// "virt-api unavailable" without parsing error strings. The full
// detail stays in the "error" field for humans — this is just an
// index.
//
// The match is on *apierrors.StatusError specifically, whose
// ErrStatus.Code carries the HTTP status the apiserver returned.
// wrapping layers (fmt.Errorf with %w in runAction, in the action's
// own Run closure) are transparent to errors.As, so the label works
// regardless of how many wraps an error has accumulated by the time
// it reaches the handler.
func apiserverErrorLabel(err error) string {
	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) {
		return errLabelOther
	}

	switch statusErr.ErrStatus.Code {
	case http.StatusForbidden:
		return errLabelForbidden
	case http.StatusNotFound:
		return errLabelNotFound
	case http.StatusUnauthorized:
		return errLabelUnauthorized
	}

	return errLabelOther
}
