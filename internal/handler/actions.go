package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
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
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.actionLookup", map[string]any{"Name": appName}))

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
func (pgh *PageHandler) runAction(
	ctx context.Context, usr *auth.UserContext,
	action actions.Action, tenantNS, appName string,
) error {
	userCfg, err := k8s.BuildUserRESTConfig(pgh.baseCfg, usr, pgh.authMode)
	if err != nil {
		return fmt.Errorf("building user rest config for action: %w", err)
	}

	runErr := action.Run(ctx, userCfg, tenantNS, appName)
	if runErr != nil {
		return fmt.Errorf("running %s action: %w", action.ID, runErr)
	}

	return nil
}

// finishAction writes the audit event and the user-visible toast for
// both the success and error paths. Extracted from InvokeAction so
// the latter stays under the funlen budget now that the same tail
// is reached from two separate branches.
func (pgh *PageHandler) finishAction(
	writer http.ResponseWriter, req *http.Request, usr *auth.UserContext,
	action actions.Action, kind, tenantNS, appName, actionID string, runErr error,
) {
	if runErr != nil {
		pgh.log.Warn("action failed",
			"tenant", tenantNS, "name", appName, "action", actionID, "error", runErr)
		pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
			audit.OutcomeError, map[string]any{
				"action":  action.AuditCategory,
				"kind":    kind,
				"error":   runErr.Error(),
				"runtime": apiserverErrorLabel(runErr),
			})
		pgh.renderErrorToast(writer, req,
			pgh.t(req, "error.app.actionFailed", map[string]any{"Action": actionID}))

		return
	}

	pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
		audit.OutcomeSuccess, map[string]any{
			"action": action.AuditCategory,
			"kind":   kind,
		})

	pgh.emitSuccessToast(writer, req,
		pgh.t(req, "toast.app.actionQueued", map[string]any{
			"Name":   appName,
			"Action": actionID,
		}))
}

// apiserverErrorLabel gives the audit log a compact machine-readable
// tag for the failure family (forbidden / notFound / network / other)
// so log queries can group "denied by RBAC" separately from "virt-api
// unavailable" without parsing error strings. The full detail stays in
// the "error" field for humans — this is just an index.
func apiserverErrorLabel(err error) string {
	var statusErr interface {
		Status() interface{ GetCode() int32 }
	}

	if errors.As(err, &statusErr) {
		switch statusErr.Status().GetCode() {
		case http.StatusForbidden:
			return "forbidden"
		case http.StatusNotFound:
			return "notFound"
		case http.StatusUnauthorized:
			return "unauthorized"
		}
	}

	return "other"
}
