package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// TestAppFormYAMLFragmentRequiresAuth pins the 401 guard — a
// misconfigured route without the middleware must not leak
// form-sourced spec YAML to an anonymous caller.
func TestAppFormYAMLFragmentRequiresAuth(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/fragments/app-yaml", nil)

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLFragment(rec, req)

	// requireUser renders an error page instead of writing
	// 401 — either a non-200 status or a redirect/HTML error
	// body proves the guard fired. Accept anything that's
	// clearly not a rendered YAML response.
	if rec.Code == http.StatusOK {
		t.Errorf("status = %d, want non-OK for anonymous caller", rec.Code)
	}
}

// TestAppFormYAMLFragmentEmptyKindReturnsBlank confirms the
// no-kind branch returns an empty textarea body rather than a
// 400 or a schema-fetch against "". The create modal renders
// the YAML tab before the user picks a Kind, so a GET with no
// kind is the expected first paint, not an error.
func TestAppFormYAMLFragmentEmptyKindReturnsBlank(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	form := strings.NewReader("name=pg")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 on empty-kind branch", rec.Code)
	}

	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty on empty-kind", rec.Body.String())
	}
}

// TestAppFormYAMLToFormFragmentRequiresKind pins the 400
// guard for the apply-to-form endpoint: without a kind the
// server can't fetch a schema and can't render fields. The
// client-side hx-include is expected to ship kind every time;
// a missing value is a programming bug the handler should
// surface loudly.
func TestAppFormYAMLToFormFragmentRequiresKind(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	form := strings.NewReader("spec_yaml=replicas%3A+3")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml-to-form", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLToFormFragment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on missing kind", rec.Code)
	}
}

// withFragmentTestUser attaches a user to the request context
// so the requireUser guard in each fragment handler clears and
// the test can reach the code path under examination. The
// user identity itself is inert — no RBAC check runs before
// the branches these tests target.
func withFragmentTestUser(t *testing.T) context.Context {
	t.Helper()

	return auth.ContextWithUser(t.Context(),
		&auth.UserContext{Username: "test-user"})
}
