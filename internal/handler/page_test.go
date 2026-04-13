package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// fakeSchemaLister is a minimal schemaLister stand-in. Returning a
// fixed response + optional error lets tests isolate resolveKindParam's
// behaviour from the real SchemaService.
type fakeSchemaLister struct {
	schemas []k8s.AppSchema
	err     error
	calls   int
}

func (f *fakeSchemaLister) List(_ context.Context, _ *auth.UserContext) ([]k8s.AppSchema, error) {
	f.calls++

	return f.schemas, f.err
}

func silentLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestSelectKnownKind is the single gate that keeps user-controlled
// query params from flowing into rendered URLs and the create-app
// modal. Only values that exactly match a known AppSchema kind pass
// through; anything else — empty, unknown, injection-crafted —
// collapses to "".
func TestSelectKnownKind(t *testing.T) {
	t.Parallel()

	schemas := []k8s.AppSchema{
		{Kind: "Etcd"},
		{Kind: "Redis"},
	}

	cases := map[string]string{
		"":              "",     // empty stays empty
		"Etcd":          "Etcd", // exact match accepted
		"Redis":         "Redis",
		"Postgres":      "", // unknown kind rejected
		"Etcd&evil=1":   "", // URL injection rejected
		"etcd":          "", // case-sensitive
		"Etcd ":         "", // no trimming
		" Etcd":         "", // no trimming
		"<script>":      "", // HTML payload rejected
		"Etcd/../admin": "", // path traversal rejected
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			got := selectKnownKind(input, schemas)
			if got != want {
				t.Errorf("selectKnownKind(%q, schemas) = %q, want %q", input, got, want)
			}
		})
	}
}

// TestSelectKnownKindEmptySchemas makes sure the function does not
// panic on a nil / empty schema list — schemaSvc.List errors are
// logged and dropped in the caller, so an empty slice is a real code
// path, not just a test fixture.
func TestSelectKnownKindEmptySchemas(t *testing.T) {
	t.Parallel()

	if got := selectKnownKind("Etcd", nil); got != "" {
		t.Errorf("selectKnownKind(%q, nil) = %q, want empty", "Etcd", got)
	}

	if got := selectKnownKind("Etcd", []k8s.AppSchema{}); got != "" {
		t.Errorf("selectKnownKind(%q, []) = %q, want empty", "Etcd", got)
	}
}

// TestResolveKindParamValidKind exercises the happy path: a kind the
// user sends matches a schema the lister returns, so it flows through
// unchanged.
func TestResolveKindParamValidKind(t *testing.T) {
	t.Parallel()

	lister := &fakeSchemaLister{schemas: []k8s.AppSchema{{Kind: "Etcd"}, {Kind: "Redis"}}}

	got := resolveKindParam(context.Background(), nil, "Etcd", lister, silentLogger())
	if got != "Etcd" {
		t.Errorf("resolveKindParam(Etcd) = %q, want Etcd", got)
	}
	if lister.calls != 1 {
		t.Errorf("expected 1 List call, got %d", lister.calls)
	}
}

// TestResolveKindParamUnknownKind makes sure an otherwise-valid-looking
// but unregistered kind is dropped rather than propagated to the view.
func TestResolveKindParamUnknownKind(t *testing.T) {
	t.Parallel()

	lister := &fakeSchemaLister{schemas: []k8s.AppSchema{{Kind: "Etcd"}}}

	got := resolveKindParam(context.Background(), nil, "Postgres", lister, silentLogger())
	if got != "" {
		t.Errorf("resolveKindParam(Postgres) = %q, want empty", got)
	}
}

// TestResolveKindParamSchemaListError locks in the fail-closed posture:
// when the lister errors, no kind passes validation, even a nominally
// well-formed one. If this ever regresses the user would see a stale /
// unverified kind rendered back to them.
func TestResolveKindParamSchemaListError(t *testing.T) {
	t.Parallel()

	lister := &fakeSchemaLister{
		schemas: []k8s.AppSchema{{Kind: "Etcd"}}, // would otherwise allow Etcd
		err:     errors.New("boom"),
	}

	got := resolveKindParam(context.Background(), nil, "Etcd", lister, silentLogger())
	if got != "" {
		t.Errorf("resolveKindParam on List error = %q, want empty (fail-closed)", got)
	}
}

// TestResolveKindParamNilListerPassesThrough covers the opt-out path
// callers use when the downstream renderer is already escape-safe and
// schema validation would just cost an API round-trip. Pass-through
// is intentional and must not silently start dropping values if a
// future refactor flips the nil-check.
func TestResolveKindParamNilListerPassesThrough(t *testing.T) {
	t.Parallel()

	got := resolveKindParam(context.Background(), nil, "Etcd", nil, silentLogger())
	if got != "Etcd" {
		t.Errorf("resolveKindParam(nil lister) = %q, want pass-through Etcd", got)
	}
}

// TestResolveKindParamEmptyParamSkipsList is both a correctness check
// (empty in → empty out) and a cost check — the common navigation to
// /tenants without ?kind= must not trigger a schemaSvc round-trip.
func TestResolveKindParamEmptyParamSkipsList(t *testing.T) {
	t.Parallel()

	lister := &fakeSchemaLister{schemas: []k8s.AppSchema{{Kind: "Etcd"}}}

	got := resolveKindParam(context.Background(), nil, "", lister, silentLogger())
	if got != "" {
		t.Errorf("resolveKindParam(empty) = %q, want empty", got)
	}
	if lister.calls != 0 {
		t.Errorf("expected 0 List calls on empty input, got %d", lister.calls)
	}
}

// TestBuildTenantPageDataReadsCreateKindParam locks in the literal
// query-param name buildTenantPageData reads from. A typo in the
// param name (createKind → create_kind, etc.) would silently break
// the marketplace flow: every test in this package would still pass
// because the field would just always be empty. Test the read path
// directly via the same helper buildTenantPageData uses.
func TestBuildTenantPageDataReadsCreateKindParam(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/?createKind=Etcd":     "Etcd",
		"/?createKind=Postgres": "", // unknown — selectKnownKind drops
		"/?createKind=":         "", // empty value
		"/":                     "", // missing param
		// Common typo guard: if the production code ever drifts to
		// reading "create_kind" or "kind", this test would still
		// flag the regression because the assertion expects the
		// "createKind" spelling exclusively.
		"/?create_kind=Etcd": "",
		"/?kind=Etcd":        "",
	}

	schemas := []k8s.AppSchema{{Kind: "Etcd"}}

	for rawURL, want := range cases {
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), "GET", rawURL, nil)
			got := extractCreateKindFromQuery(req, schemas)

			if got != want {
				t.Errorf("extractCreateKindFromQuery for %q = %q, want %q", rawURL, got, want)
			}
		})
	}
}

// TestCreateKindQueryParamConstant pins the literal param name. If
// production drifts to a different spelling, this test fails — the
// extractCreateKindFromQuery callers and the marketplace flow all use
// the same constant.
func TestCreateKindQueryParamConstant(t *testing.T) {
	t.Parallel()

	if createKindQueryParam != "createKind" {
		t.Errorf("createKindQueryParam = %q, want %q — renaming this breaks every marketplace card link", createKindQueryParam, "createKind")
	}
}
