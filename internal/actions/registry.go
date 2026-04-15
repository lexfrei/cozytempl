// Package actions holds the per-resource action registry — the set
// of named operations each Cozystack application Kind exposes beyond
// plain CRUD on the application CR itself. First consumer is the
// VMInstance Kind (KubeVirt VM Start / Stop / Restart), but the
// registry is deliberately not VM-specific: the same shape works
// for CNPG "manual backup trigger", Redis "failover", etc.
//
// The registry is explicit and compile-time — not derived from the
// ApplicationDefinition openAPISchema — because actions have
// side-effects an openAPISchema cannot express (target resource
// shape, subresource verb, audit action category). Schema-driven
// form rendering stays the sole source of truth for editable
// fields.
package actions

import (
	"context"

	"k8s.io/client-go/rest"
)

// Action is one registered operation. ID is URL-safe (lowercase
// alphanumerics + hyphens); LabelKey maps to an i18n bundle entry
// so the UI renders in the user's locale; AuditCategory identifies
// the action in the audit stream (see internal/audit).
type Action struct {
	ID            string
	LabelKey      string
	AuditCategory string
	// Run is the side-effecting implementation. The handler builds
	// a user-credentialed *rest.Config (via k8s.BuildUserRESTConfig)
	// and passes it in, so Run never touches auth modes directly —
	// it just talks to the apiserver as the current user.
	// Implementations must NOT touch the request context's cookies
	// or writer — this is strictly a server-side k8s call.
	Run func(ctx context.Context, userCfg *rest.Config, namespace, name string) error
}

// byKind is the package-level registry. Callers read it by kind to
// enumerate buttons for the detail page. Mutated only by init-time
// Register calls; never accept writes from HTTP handlers.
//
//nolint:gochecknoglobals // intentional init-time registry
var byKind = map[string][]Action{}

// Register attaches an action to a Cozystack application Kind. Safe
// to call from init() functions; panics on an empty Kind or ID so
// wiring bugs surface at startup rather than at button-click time.
func Register(kind string, action Action) {
	if kind == "" {
		panic("actions.Register: empty kind")
	}

	if action.ID == "" {
		panic("actions.Register: empty action ID")
	}

	byKind[kind] = append(byKind[kind], action)
}

// For returns every action registered for the given Cozystack
// application Kind. A nil slice means "no actions registered" and
// is the normal result for the majority of Kinds that expose no
// subresources — the UI renders no action bar in that case.
func For(kind string) []Action {
	return byKind[kind]
}

// Lookup finds one action by Kind + ID. Returns (action, true) on
// hit, zero-value + false on miss. Used by the action POST handler
// to translate an HTTP path segment into a Run callable.
func Lookup(kind, id string) (Action, bool) {
	for _, action := range byKind[kind] {
		if action.ID == id {
			return action, true
		}
	}

	return Action{}, false
}
