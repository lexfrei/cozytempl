package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAuth_Authenticated(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	// Create a session with user data
	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tenants", nil)

	session, err := store.Get(req)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	SetUser(session, testUsername, []string{"tenant-root-admin"}, "token")

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
	RequireAuth(store, inner).ServeHTTP(rec2, req2)

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

	RequireAuth(store, inner).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	if called {
		t.Error("inner handler should not be called for unauthenticated requests")
	}
}

func TestUserFromContext_Missing(t *testing.T) {
	ctx := context.Background()

	usr := UserFromContext(ctx)
	if usr != nil {
		t.Errorf("expected nil user, got %+v", usr)
	}
}
