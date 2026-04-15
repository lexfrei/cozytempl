package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
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

// appGetter is the narrow slice of ApplicationService that
// InvokeAction needs. Kept local to the handler package so tests
// can inject a stub without dragging in the full concrete type.
// Production wiring in NewPageHandler stores deps.AppSvc here —
// ApplicationService satisfies this interface by construction.
type appGetter interface {
	Get(ctx context.Context, usr *auth.UserContext, namespace, name string) (*k8s.Application, error)
}

// actionIDPattern matches the URL-safe action ID spelling declared
// by the registry docstring: lowercase alphanumerics and hyphens
// only. Validated before the path value flows into audit fields,
// log lines, or toast template data so a crafted path can't smuggle
// a <script> fragment into the UI (templ auto-escapes anyway, but
// the rendered copy reads terribly with arbitrary strings in it).
var actionIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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

	if !k8s.IsValidLabelValue(tenantNS) || !k8s.IsValidLabelValue(appName) || !actionIDPattern.MatchString(actionID) {
		// Do NOT echo the raw actionID into the toast — the value is
		// attacker-controlled and reads terribly when it contains
		// anything other than a registered slug. The generic
		// actionUnknown copy is the right landing for malformed
		// input; audit captures the raw value for operators.
		pgh.log.Warn("action invoked with malformed path value",
			"tenant", tenantNS, "name", appName, "action", actionID)
		pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
			audit.OutcomeError, map[string]any{
				"error":     "malformed path value",
				"rawAction": actionID,
			})
		pgh.renderErrorToast(writer, req,
			pgh.t(req, "error.app.actionUnknown", map[string]any{"Action": "?"}))

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
	app, err := pgh.appGetter.Get(req.Context(), usr, tenantNS, appName)
	if err != nil {
		pgh.log.Error("getting app for action", "tenant", tenantNS, "name", appName, "error", err)

		label := apiserverErrorLabel(err)

		// Audit every lookup failure so compliance queries can tell
		// "someone tried to act on a resource they can't see" from
		// "someone tried to act on a resource that no longer exists"
		// without scraping debug logs. The per-branch toast copy is
		// separate from the audit; both emit regardless.
		pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
			audit.OutcomeError, map[string]any{
				"error":      err.Error(),
				"errorClass": label,
				"stage":      "lookup",
			})

		switch label {
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
		// Audit dispatch misses too — a valid-shape action ID that
		// isn't registered under the app's Kind is a probing pattern
		// operators should be able to grep for. Matches the audit
		// coverage on malformed and lookup-failure branches above.
		pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
			audit.OutcomeError, map[string]any{
				"error":     "unknown action",
				"kind":      app.Kind,
				"stage":     "dispatch",
				"rawAction": actionID,
			})
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
// Both paths emit ONLY an out-of-band toast — no main-content swap.
// The page the user is looking at stays in place; the live-watch
// stream (see internal/k8s watchers) pushes the post-action status
// update through SSE when the apiserver reconciler catches up. This
// avoids the stale-render window where a fresh re-fetch returns the
// pre-reconciliation status (KubeVirt's /start is synchronous at the
// apiserver but the VMI status field takes a reconcile loop to flip).
//
// Hx-Reswap: none is the signal that the OOB toast in the response
// body must NOT be copied into #main-content. Without it, htmx would
// swap the toast div in, blanking the detail page. Set unconditionally
// on both paths so the ordering relative to toast-writing doesn't
// matter even if a future middleware eagerly flushes headers.
//
//nolint:gocritic // hugeParam: one copy per HTTP request, well off the hot path
func (pgh *PageHandler) finishAction(
	writer http.ResponseWriter, req *http.Request, usr *auth.UserContext,
	action actions.Action, kind, tenantNS, appName, actionID string, runErr error,
) {
	writer.Header().Set("Hx-Reswap", "none")

	label := pgh.localizedActionLabel(req, action, actionID)

	if runErr != nil {
		pgh.log.Warn("action failed",
			"tenant", tenantNS, "name", appName, "action", actionID, "error", runErr)
		pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
			audit.OutcomeError, map[string]any{
				"subaction":  action.AuditCategory,
				"kind":       kind,
				"error":      runErr.Error(),
				"errorClass": apiserverErrorLabel(runErr),
			})
		pgh.renderErrorToast(writer, req,
			pgh.t(req, "error.app.actionFailed", map[string]any{"Action": label}))

		return
	}

	pgh.recordAudit(req, usr, audit.ActionAppAction, appName, tenantNS,
		audit.OutcomeSuccess, map[string]any{
			"subaction": action.AuditCategory,
			"kind":      kind,
		})

	pgh.emitSuccessToast(writer, req,
		pgh.t(req, "toast.app.actionSucceeded", map[string]any{
			"Name":   appName,
			"Action": label,
		}))
}

// localizedActionLabel translates the action's LabelKey via the
// request-scoped Localizer so toasts read "Остановить" / "停止" /
// "Стоп" rather than the URL slug. Falls back to the raw actionID
// when go-i18n's missing-key fallback format ('[page.foo.bar]')
// surfaces — a defensive step so a broken locale never bakes the
// brackets into a user-facing string.
//
//nolint:gocritic // hugeParam: one invocation per POST, off the hot path
func (pgh *PageHandler) localizedActionLabel(req *http.Request, action actions.Action, actionID string) string {
	label := pgh.t(req, action.LabelKey)

	if label == "["+action.LabelKey+"]" {
		return actionID
	}

	return label
}

// apiserverErrorLabel gives the audit log a compact machine-readable
// tag for the failure family (forbidden / notFound / unauthorized /
// other) so log queries can group "denied by RBAC" separately from
// "virt-api unavailable" without parsing error strings. The full
// detail stays in the "error" field for humans — this is just an
// index.
//
// Returns "" for a nil error so callers can omit the errorClass
// field from audit details on the success path. Callers that do
// pass non-nil errors get one of the four non-empty labels; an
// operator filtering "errorClass=other" sees only real
// unclassified failures, never accidental nil-pass noise.
//
// The match is on *apierrors.StatusError specifically, whose
// ErrStatus.Code carries the HTTP status the apiserver returned.
// wrapping layers (fmt.Errorf with %w in runAction, in the action's
// own Run closure) are transparent to errors.As, so the label works
// regardless of how many wraps an error has accumulated by the time
// it reaches the handler.
func apiserverErrorLabel(err error) string {
	if err == nil {
		return ""
	}

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
