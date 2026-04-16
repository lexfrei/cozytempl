package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// TestPaletteHandlerRequiresAuth confirms the 401 guard. Without
// it any anonymous caller could enumerate tenants and apps the
// logged-in user can see. The palette is a JSON API under /api
// so it sits inside the protect() middleware in production, but
// the handler's own guard is belt-and-braces.
func TestPaletteHandlerRequiresAuth(t *testing.T) {
	t.Parallel()

	handler := NewPaletteHandler(nil, nil, slog.New(slog.DiscardHandler))

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet, "/api/palette-index", nil)

	rec := httptest.NewRecorder()
	handler.Index(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestPaletteIndexShape locks the JSON field names the client
// depends on. Renaming `namespace`, `displayName`, `name`,
// `tenant`, or `kind` silently breaks static/ts/palette.ts
// without any TypeScript or Go compile error.
func TestPaletteIndexShape(t *testing.T) {
	t.Parallel()

	index := PaletteIndex{
		Tenants: []PaletteTenant{{Namespace: "ns-a", DisplayName: "Tenant A"}},
		Apps:    []PaletteApp{{Name: "pg", Tenant: "ns-a", Kind: "Postgres"}},
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
	} {
		if !strings.Contains(got, needle) {
			t.Errorf("JSON missing required key %s: %s", needle, got)
		}
	}
}

// withPaletteTestUser attaches a UserContext to the request so
// handler tests clear the 401 guard and hit the data-assembly
// path. The username identifies the caller in logs only; it does
// not influence RBAC — tenantSvc / appSvc mocks control that.
func withPaletteTestUser(ctx context.Context) context.Context {
	return auth.ContextWithUser(ctx, &auth.UserContext{Username: "test-user"})
}

// TestPaletteHandlerUnauthenticatedUsesHelper sanity-checks the
// helper above so the rest of the suite can rely on it.
func TestPaletteHandlerUnauthenticatedUsesHelper(t *testing.T) {
	t.Parallel()

	ctx := withPaletteTestUser(context.Background())
	if auth.UserFromContext(ctx) == nil {
		t.Error("withPaletteTestUser did not attach a UserContext")
	}
}
