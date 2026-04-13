package handler

import (
	"context"
	"errors"
	"log/slog"
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

	lister := &fakeSchemaLister{err: errors.New("boom")}

	got := resolveKindParam(context.Background(), nil, "Etcd", lister, silentLogger())
	if got != "" {
		t.Errorf("resolveKindParam on List error = %q, want empty (fail-closed)", got)
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
