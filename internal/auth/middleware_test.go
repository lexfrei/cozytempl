package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lexfrei/cozytempl/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestRequireAuth_Authenticated(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	// Create a session with user data
	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)

	session, err := store.Get(req)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	SetUser(session, &UserSession{
		Username: testUsername,
		Groups:   []string{"tenant-root-admin"},
		IDToken:  "token",
	})

	err = store.Save(req, recorder, session)
	if err != nil {
		t.Fatalf("save session: %v", err)
	}

	// Build a request with the session cookie
	cookies := recorder.Result().Cookies()
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)

	for _, cookie := range cookies {
		req2.AddCookie(cookie)
	}

	var capturedUser *UserContext

	inner := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		capturedUser = UserFromContext(req.Context())
	})

	rec2 := httptest.NewRecorder()
	RequireAuth(store, nil, testLogger(), config.AuthModeImpersonationLegacy, inner).ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec2.Code, http.StatusOK)
	}

	if capturedUser == nil {
		t.Fatal("expected user in context, got nil")
	}

	if capturedUser.Username != testUsername {
		t.Errorf("username = %q, want %q", capturedUser.Username, testUsername)
	}
}

func TestRequireAuth_Unauthenticated(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)
	recorder := httptest.NewRecorder()

	called := false

	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	RequireAuth(store, nil, testLogger(), config.AuthModeImpersonationLegacy, inner).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	if called {
		t.Error("inner handler should not be called for unauthenticated requests")
	}
}

func TestRequireAuth_TokenMode_Authenticated(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)

	session, err := store.Get(req)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	SetBearerToken(session, "abc.def.ghi")

	if err := store.Save(req, recorder, session); err != nil {
		t.Fatalf("save session: %v", err)
	}

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)
	for _, cookie := range recorder.Result().Cookies() {
		req2.AddCookie(cookie)
	}

	var capturedUser *UserContext

	inner := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		capturedUser = UserFromContext(req.Context())
	})

	rec2 := httptest.NewRecorder()
	RequireAuth(store, nil, testLogger(), config.AuthModeToken, inner).ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec2.Code, http.StatusOK)
	}

	if capturedUser == nil {
		t.Fatal("expected user in context, got nil")
	}

	if capturedUser.BearerToken != "abc.def.ghi" {
		t.Errorf("BearerToken = %q, want %q", capturedUser.BearerToken, "abc.def.ghi")
	}

	if capturedUser.Username != "token-user" {
		t.Errorf("Username = %q, want token-user", capturedUser.Username)
	}
}

func TestRequireAuth_TokenMode_MissingTokenRedirects(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()

	called := false

	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	RequireAuth(store, nil, testLogger(), config.AuthModeToken, inner).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusFound {
		t.Errorf("status = %d, want %d (redirect to /auth/token)", recorder.Code, http.StatusFound)
	}

	if loc := recorder.Header().Get("Location"); loc != "/auth/token" {
		t.Errorf("Location = %q, want /auth/token", loc)
	}

	if called {
		t.Error("inner handler should not be called when token is missing")
	}
}

func TestRequireAuth_TokenMode_MissingTokenAPIReturns401(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)
	recorder := httptest.NewRecorder()

	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})

	RequireAuth(store, nil, testLogger(), config.AuthModeToken, inner).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for /api/* with no token", recorder.Code)
	}
}

func TestUserFromContext_Missing(t *testing.T) {
	ctx := context.Background()

	usr := UserFromContext(ctx)
	if usr != nil {
		t.Errorf("expected nil user, got %+v", usr)
	}
}
