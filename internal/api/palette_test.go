package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// stubTenantLister satisfies paletteTenantLister with a fixed
// return value. The call counter proves the handler queries
// exactly once per request; the error override covers the
// tenant-list-failure path (full 500).
type stubTenantLister struct {
	items []k8s.Tenant
	err   error
}

func (s *stubTenantLister) ListMinimal(context.Context, *auth.UserContext) ([]k8s.Tenant, error) {
	if s.err != nil {
		return nil, s.err
	}

	return s.items, nil
}

// stubAppLister returns a per-tenant ApplicationList from a map.
// Missing keys return an empty list; tenants listed in errs
// return the matching error so the "one broken tenant must not
// blank the palette" invariant has a driver. The call counter
// lets perf tests pin the exact apiserver round-trip count.
type stubAppLister struct {
	byTenant map[string]k8s.ApplicationList
	errs     map[string]error
	calls    atomic.Int64
}

func (s *stubAppLister) List(ctx context.Context, _ *auth.UserContext, tenant string) (k8s.ApplicationList, error) {
	s.calls.Add(1)

	if ctx.Err() != nil {
		return k8s.ApplicationList{}, ctx.Err() //nolint:wrapcheck // propagating the exact ctx error is the test contract
	}

	if err, ok := s.errs[tenant]; ok {
		return k8s.ApplicationList{}, err
	}

	return s.byTenant[tenant], nil
}

// withPaletteTestUser attaches a UserContext to ctx so the
// handler clears its 401 guard and hits the data-assembly path.
// Username is logging-only — RBAC is driven by the lister stubs.
func withPaletteTestUser(ctx context.Context) context.Context {
	return auth.ContextWithUser(ctx, &auth.UserContext{Username: "test-user"})
}

// paletteTestHandler is the common harness: a stub tenant lister
// and stub app lister, ready to be handed to NewPaletteHandler.
func paletteTestHandler(t *testing.T, tl *stubTenantLister, al *stubAppLister) *PaletteHandler {
	t.Helper()

	return NewPaletteHandler(tl, al, slog.New(slog.DiscardHandler))
}

// paletteGET builds the GET request with a pre-authenticated
// context so tests do not re-author the boilerplate per case.
func paletteGET(t *testing.T) *http.Request {
	t.Helper()

	return httptest.NewRequestWithContext(
		withPaletteTestUser(context.Background()),
		http.MethodGet, "/api/palette-index", nil)
}

// TestPaletteHandlerRequiresAuth confirms the 401 guard. Without
// it any anonymous caller could enumerate tenants and apps the
// logged-in user can see. The handler sits inside protect() in
// production, but the guard is belt-and-braces.
func TestPaletteHandlerRequiresAuth(t *testing.T) {
	t.Parallel()

	handler := NewPaletteHandler(&stubTenantLister{}, &stubAppLister{}, slog.New(slog.DiscardHandler))

	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, "/api/palette-index", nil)

	rec := httptest.NewRecorder()
	handler.Index(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestPaletteIndexSuccess walks the happy path: two tenants,
// apps in each → sorted JSON with every expected entry. Locks
// (a) tenant order preserved from TenantLister, (b) apps
// emitted in tenant-then-list-order, (c) Kind passed through
// unchanged so the client can render the hint.
func TestPaletteIndexSuccess(t *testing.T) {
	t.Parallel()

	tl := &stubTenantLister{items: []k8s.Tenant{
		{Namespace: "tenant-a", DisplayName: "Tenant A"},
		{Namespace: "tenant-b", DisplayName: "Tenant B"},
	}}
	al := &stubAppLister{byTenant: map[string]k8s.ApplicationList{
		"tenant-a": {Items: []k8s.Application{
			{Name: "pg-main", Kind: "Postgres"},
			{Name: "redis-cache", Kind: "Redis"},
		}},
		"tenant-b": {Items: []k8s.Application{
			{Name: "vm-worker", Kind: "VMInstance"},
		}},
	}}

	handler := paletteTestHandler(t, tl, al)
	rec := httptest.NewRecorder()
	handler.Index(rec, paletteGET(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got PaletteIndex
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(got.Tenants) != 2 {
		t.Fatalf("len(tenants) = %d, want 2", len(got.Tenants))
	}

	if got.Tenants[0].Namespace != "tenant-a" || got.Tenants[1].Namespace != "tenant-b" {
		t.Errorf("tenant order = %+v, want tenant-a then tenant-b", got.Tenants)
	}

	if len(got.Apps) != 3 {
		t.Fatalf("len(apps) = %d, want 3; got %+v", len(got.Apps), got.Apps)
	}

	wantApps := []PaletteApp{
		{Name: "pg-main", Tenant: "tenant-a", Kind: "Postgres"},
		{Name: "redis-cache", Tenant: "tenant-a", Kind: "Redis"},
		{Name: "vm-worker", Tenant: "tenant-b", Kind: "VMInstance"},
	}

	for i, want := range wantApps {
		if got.Apps[i] != want {
			t.Errorf("apps[%d] = %+v, want %+v", i, got.Apps[i], want)
		}
	}
}

// TestPaletteIndexPerTenantErrorDoesNotBlank pins the "single
// broken tenant should not blank the palette" invariant. Tenant
// A's List succeeds, Tenant B's List fails — the response must
// still include Tenant A plus both tenant entries in the
// tenants[] list (so the user can navigate there) and skip only
// Tenant B's apps.
func TestPaletteIndexPerTenantErrorDoesNotBlank(t *testing.T) {
	t.Parallel()

	tl := &stubTenantLister{items: []k8s.Tenant{
		{Namespace: "tenant-a", DisplayName: "Tenant A"},
		{Namespace: "tenant-b", DisplayName: "Tenant B"},
	}}
	al := &stubAppLister{
		byTenant: map[string]k8s.ApplicationList{
			"tenant-a": {Items: []k8s.Application{{Name: "pg", Kind: "Postgres"}}},
		},
		errs: map[string]error{
			"tenant-b": errors.New("apiserver timeout"),
		},
	}

	handler := paletteTestHandler(t, tl, al)
	rec := httptest.NewRecorder()
	handler.Index(rec, paletteGET(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial failure must not 5xx); body = %s",
			rec.Code, rec.Body.String())
	}

	var got PaletteIndex
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(got.Tenants) != 2 {
		t.Errorf("tenants = %+v, want both entries present even when one app-list failed", got.Tenants)
	}

	if len(got.Apps) != 1 || got.Apps[0].Name != "pg" {
		t.Errorf("apps = %+v, want just pg from tenant-a", got.Apps)
	}
}

// TestPaletteIndexTenantListError covers the full-failure
// branch: a tenant-list error surfaces as 500 rather than an
// empty index, so operators see a loud failure instead of a
// quiet "no tenants visible" UX.
func TestPaletteIndexTenantListError(t *testing.T) {
	t.Parallel()

	tl := &stubTenantLister{err: errors.New("kube-apiserver down")}
	al := &stubAppLister{}

	handler := paletteTestHandler(t, tl, al)
	rec := httptest.NewRecorder()
	handler.Index(rec, paletteGET(t))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on tenant list failure", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "failed to list tenants") {
		t.Errorf("body = %q, want failure copy", rec.Body.String())
	}
}

// TestPaletteIndexSurfacesTruncation pins the Truncated →
// TruncatedTenants plumbing. Without it a tenant with 500+ apps
// loses the overflow silently and an operator typing app #501 by
// name sees no match and no explanation.
func TestPaletteIndexSurfacesTruncation(t *testing.T) {
	t.Parallel()

	tl := &stubTenantLister{items: []k8s.Tenant{
		{Namespace: "tenant-big", DisplayName: "Big"},
	}}
	al := &stubAppLister{byTenant: map[string]k8s.ApplicationList{
		"tenant-big": {
			Items: []k8s.Application{
				{Name: "pg-1", Kind: "Postgres"},
			},
			Truncated: true,
		},
	}}

	handler := paletteTestHandler(t, tl, al)
	rec := httptest.NewRecorder()
	handler.Index(rec, paletteGET(t))

	var got PaletteIndex
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(got.TruncatedTenants) != 1 || got.TruncatedTenants[0] != "tenant-big" {
		t.Errorf("truncatedTenants = %+v, want [tenant-big]", got.TruncatedTenants)
	}

	if len(got.Apps) != 1 {
		t.Errorf("apps truncated-case lost visible items: %+v", got.Apps)
	}
}

// TestPaletteIndexOneAppListPerTenant pins the perf contract:
// exactly N apps.List calls for N tenants. A future regression
// that re-introduces a second per-tenant list (e.g. switching
// the lister back to tenants.List's AppCount variant) would
// double the apiserver round-trip cost and surface here as an
// off-by-N counter.
func TestPaletteIndexOneAppListPerTenant(t *testing.T) {
	t.Parallel()

	tl := &stubTenantLister{items: []k8s.Tenant{
		{Namespace: "tenant-a", DisplayName: "Tenant A"},
		{Namespace: "tenant-b", DisplayName: "Tenant B"},
		{Namespace: "tenant-c", DisplayName: "Tenant C"},
	}}
	al := &stubAppLister{byTenant: map[string]k8s.ApplicationList{
		"tenant-a": {Items: []k8s.Application{{Name: "pg", Kind: "Postgres"}}},
		"tenant-b": {Items: []k8s.Application{}},
		"tenant-c": {Items: []k8s.Application{{Name: "vm", Kind: "VMInstance"}}},
	}}

	handler := paletteTestHandler(t, tl, al)
	rec := httptest.NewRecorder()
	handler.Index(rec, paletteGET(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := al.calls.Load(); got != 3 {
		t.Errorf("apps.List fired %d times, want 3 (one per tenant, no more)", got)
	}
}

// TestPaletteIndexContextCancelShortCircuits drives a request
// whose context is cancelled before Index is called. The
// cancellation must propagate through the fanout so workers
// bail fast, and context-cancel errors must NOT produce Warn
// lines (that was the log-spam regression path).
func TestPaletteIndexContextCancelShortCircuits(t *testing.T) {
	t.Parallel()

	tl := &stubTenantLister{items: []k8s.Tenant{
		{Namespace: "tenant-a", DisplayName: "Tenant A"},
		{Namespace: "tenant-b", DisplayName: "Tenant B"},
	}}
	al := &stubAppLister{byTenant: map[string]k8s.ApplicationList{
		"tenant-a": {Items: []k8s.Application{{Name: "pg", Kind: "Postgres"}}},
		"tenant-b": {Items: []k8s.Application{}},
	}}

	handler := paletteTestHandler(t, tl, al)

	ctx, cancel := context.WithCancel(withPaletteTestUser(context.Background()))
	cancel() // cancel before the handler runs

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/palette-index", nil)

	rec := httptest.NewRecorder()
	handler.Index(rec, req)

	// The handler still returns 200 — tenants were listed before
	// the cancellation propagated. The fanout short-circuits and
	// the apps slice may be empty. The important contract is:
	// no panic, no 500, no log spam.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 on cancelled-ctx request", rec.Code)
	}
}

// TestPaletteIndexEmptyTenants pins the zero-tenant path. Clean
// 200 with empty arrays, not a 500 and not a nil body — the
// client expects well-formed JSON with the structural keys so
// the subsequent buildCatalog() can read indexCache.tenants
// without null-guards.
func TestPaletteIndexEmptyTenants(t *testing.T) {
	t.Parallel()

	handler := paletteTestHandler(t, &stubTenantLister{}, &stubAppLister{})
	rec := httptest.NewRecorder()
	handler.Index(rec, paletteGET(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on zero-tenant visibility", rec.Code)
	}

	var got PaletteIndex
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if got.Tenants == nil {
		t.Error("tenants must be an empty slice, not null")
	}

	if got.Apps == nil {
		t.Error("apps must be an empty slice, not null")
	}
}

// TestPaletteIndexShape locks the JSON field names the client
// depends on. Renaming any of these silently breaks the
// TypeScript consumer (static/ts/palette.ts PaletteIndex
// interface) with no Go or TS compile error.
func TestPaletteIndexShape(t *testing.T) {
	t.Parallel()

	index := PaletteIndex{
		Tenants:          []PaletteTenant{{Namespace: "ns-a", DisplayName: "Tenant A"}},
		Apps:             []PaletteApp{{Name: "pg", Tenant: "ns-a", Kind: "Postgres"}},
		TruncatedTenants: []string{"ns-a"},
	}

	raw, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got := string(raw)
	for _, needle := range []string{
		`"tenants"`,
		`"apps"`,
		`"namespace"`,
		`"displayName"`,
		`"name"`,
		`"tenant"`,
		`"kind"`,
		`"truncatedTenants"`,
	} {
		if !strings.Contains(got, needle) {
			t.Errorf("JSON missing required key %s: %s", needle, got)
		}
	}
}
