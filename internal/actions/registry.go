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
	"sync"
	"testing"

	"k8s.io/client-go/rest"
)

// registryMu guards byKind. Register() is designed to run at init
// time, not from HTTP handlers, so contention is effectively zero;
// the mutex exists to defend against a future test that adds
// t.Parallel() to a path touching Register and the race detector's
// absolutely-correct complaint about concurrent map writes.
//
//nolint:gochecknoglobals // lock pairs 1:1 with the registry map
var registryMu sync.RWMutex

// Capability is the (group, resource, subresource, verb) tuple the
// apiserver checks when the caller hits Run. Used by the UI-time
// capability probe so a user whose RBAC doesn't permit the
// subresource verb never sees the button — the alternative is every
// click lands a 403 toast, which reads as "cozytempl is broken"
// rather than "you don't have permission".
//
// Empty Resource disables the probe (Action always renders) — pick
// that for actions whose authorisation is not expressible as a
// single SSAR, e.g. a multi-step backend operation. Every shipped
// action MUST populate the tuple; the probe is opt-out, not opt-in.
type Capability struct {
	Group       string
	Resource    string
	Subresource string
	Verb        string
}

// HasResource returns true when Capability has a Resource set — the
// signal the probe uses to decide whether to run an SSAR at all.
// Callers should branch on this rather than comparing the Capability
// to the zero value so future fields (namespace override, etc.)
// don't silently flip the probe off.
func (c Capability) HasResource() bool {
	return c.Resource != ""
}

// Action is one registered operation. ID is URL-safe (lowercase
// alphanumerics + hyphens); LabelKey maps to an i18n bundle entry
// so the UI renders in the user's locale; AuditCategory identifies
// the action in the audit stream (see internal/audit).
type Action struct {
	ID            string
	LabelKey      string
	AuditCategory string
	// Destructive marks the action as one whose effect cannot be
	// quietly undone by a refresh — Stop powers a VM off, Restart
	// bounces a running workload. The UI renders destructive
	// actions with a confirmation prompt and a danger-variant button
	// class so an accidental click can't silently cause an outage.
	// Purely-additive actions like Start stay at Destructive=false.
	Destructive bool
	// Capability is the (group, resource, subresource, verb) tuple
	// the apiserver must accept from the caller for Run to succeed.
	// Used by UI render-time capability checks to avoid showing
	// buttons the user cannot actually click. Empty Resource
	// disables the probe — the button always renders — which is the
	// right call for actions that don't map onto a single RBAC verb.
	Capability Capability
	// TargetName translates the Cozystack application name (the
	// name the user sees and the URL path carries) into the name of
	// the actual Kubernetes resource the action POSTs against.
	// Cozystack ApplicationDefinitions declare a release prefix
	// (e.g. VMInstance uses 'vm-instance-'), so a Cozystack app
	// named 'myvm' renders a KubeVirt VM named 'vm-instance-myvm'.
	// Without this translation every action would 404 at the
	// apiserver, because the capability probe confirms permission
	// but does NOT validate object existence.
	//
	// nil means "use the app name verbatim" — the right default for
	// actions whose target lives at the release name (none shipped
	// today; the hook is here for future Kinds that behave that way).
	TargetName func(appName string) string
	// Run is the side-effecting implementation. The handler builds
	// a user-credentialed *rest.Config (via k8s.BuildUserRESTConfig)
	// and passes it in, so Run never touches auth modes directly —
	// it just talks to the apiserver as the current user.
	//
	// The name argument is the TARGET name (after TargetName
	// translation), not the Cozystack app name. For a VMInstance
	// 'myvm' Run sees 'vm-instance-myvm' — the actual KubeVirt VM
	// resource. Implementations should not re-apply prefixes.
	//
	// ctx cancellation: Run MUST honour ctx.Done(). client-go's
	// request.Do(ctx) already does, so the typical Run that ends
	// with .Do(ctx).Error() is correct; custom implementations
	// that spawn goroutines must plumb ctx through so a client
	// disconnect tears down any in-flight work.
	//
	// Caveat: in AuthModeDev the returned rest.Config carries
	// cozytempl's own service-account credentials (BuildUserRESTConfig
	// in dev mode intentionally bypasses user-identity injection),
	// so Run operates under the process SA rather than the logged-in
	// developer. That matches the rest of cozytempl's dev-mode
	// behaviour and is only a concern if an action is ever used to
	// make decisions based on the caller's identity.
	//
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
//
//nolint:gocritic // hugeParam: init-time registration, single copy per application Kind, not hot-path
func Register(kind string, action Action) {
	if kind == "" {
		panic("actions.Register: empty kind")
	}

	if action.ID == "" {
		panic("actions.Register: empty action ID")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	// Duplicate ID under the same Kind would make Lookup non-
	// deterministic (always returns the first) and render two
	// identical buttons. Fail loud at init time — matches the
	// empty-ID/empty-Kind panic semantics: wiring bugs surface at
	// startup, not at click time.
	for i := range byKind[kind] {
		if byKind[kind][i].ID == action.ID {
			panic("actions.Register: duplicate action ID " + action.ID + " for kind " + kind)
		}
	}

	byKind[kind] = append(byKind[kind], action)
}

// For returns every action registered for the given Cozystack
// application Kind. A nil slice means "no actions registered" and
// is the normal result for the majority of Kinds that expose no
// subresources — the UI renders no action bar in that case.
//
// Returns a defensive copy of the underlying slice so callers can
// iterate safely while a concurrent Register lands (test cleanup
// paths slice byKind[kind] back to a pre-test length, which races
// with a concurrent iteration of the returned slice). The registry
// is small enough that one allocation per page render is free.
func For(kind string) []Action {
	registryMu.RLock()
	defer registryMu.RUnlock()

	src := byKind[kind]
	if len(src) == 0 {
		return nil
	}

	out := make([]Action, len(src))
	copy(out, src)

	return out
}

// ResolveTargetName applies a.TargetName to the Cozystack app name,
// defaulting to the app name unchanged when no translation is set.
// Centralised here so callers don't sprinkle nil-check copies of
// this default across handler + template + test code.
//
//nolint:gocritic // hugeParam: called once per click; templ renderers and HTTP handlers expect the by-value Action
func (a Action) ResolveTargetName(appName string) string {
	if a.TargetName == nil {
		return appName
	}

	return a.TargetName(appName)
}

// RegisterForTest registers an action and returns a restore closure
// that un-registers it. Exposed so test packages outside
// internal/actions (the handler tests especially) can stage
// throwaway Kinds + IDs without leaking into later tests or into
// `go test -count=N` re-runs.
//
// Panics when called outside a test binary, matching
// SwapAllowedFnForTest: the registry is init-time production state
// and a runtime mutator is a wiring bug.
//
//nolint:gocritic // hugeParam: test-only, not on any hot path
func RegisterForTest(kind string, action Action) func() {
	if !testing.Testing() {
		panic("actions.RegisterForTest called outside a test binary")
	}

	Register(kind, action)

	return func() {
		registryMu.Lock()
		defer registryMu.Unlock()

		items := byKind[kind]
		for i := range items {
			if items[i].ID == action.ID {
				byKind[kind] = append(items[:i], items[i+1:]...)

				return
			}
		}
	}
}

// Lookup finds one action by Kind + ID. Returns (action, true) on
// hit, zero-value + false on miss. Used by the action POST handler
// to translate an HTTP path segment into a Run callable.
func Lookup(kind, actionID string) (Action, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	// Index access avoids the by-value copy on every iteration;
	// Action carries a function field plus Capability so it is large
	// enough that gocritic (rangeValCopy) flags the range-copy form.
	items := byKind[kind]
	for i := range items {
		if items[i].ID == actionID {
			return items[i], true
		}
	}

	return Action{}, false
}
