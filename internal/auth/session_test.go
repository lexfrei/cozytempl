package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionStore_RoundTrip(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	session, err := store.Get(req)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	SetUser(session, testUsername, []string{"tenant-root-admin"}, "fake-id-token")

	err = store.Save(req, recorder, session)
	if err != nil {
		t.Fatalf("save session: %v", err)
	}

	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie to be set")
	}

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	for _, cookie := range cookies {
		req2.AddCookie(cookie)
	}

	session2, err := store.Get(req2)
	if err != nil {
		t.Fatalf("get session from cookie: %v", err)
	}

	username, groups, idToken := GetUser(session2)

	if username != testUsername {
		t.Errorf("username = %q, want %q", username, testUsername)
	}

	if len(groups) != 1 || groups[0] != "tenant-root-admin" {
		t.Errorf("groups = %v, want [tenant-root-admin]", groups)
	}

	if idToken != "fake-id-token" {
		t.Errorf("idToken = %q, want %q", idToken, "fake-id-token")
	}
}

func TestSessionStore_Clear(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	session, err := store.Get(req)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	SetUser(session, testUsername, []string{"group1"}, "token")
	Clear(session)

	username, groups, idToken := GetUser(session)

	if username != "" {
		t.Errorf("username after clear = %q, want empty", username)
	}

	if groups != nil {
		t.Errorf("groups after clear = %v, want nil", groups)
	}

	if idToken != "" {
		t.Errorf("idToken after clear = %q, want empty", idToken)
	}

	err = store.Save(req, recorder, session)
	if err != nil {
		t.Fatalf("save cleared session: %v", err)
	}
}

func TestGetUser_EmptySession(t *testing.T) {
	store := NewSessionStore(testSessionKey)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	session, err := store.Get(req)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	username, groups, idToken := GetUser(session)

	if username != "" {
		t.Errorf("username = %q, want empty", username)
	}

	if groups != nil {
		t.Errorf("groups = %v, want nil", groups)
	}

	if idToken != "" {
		t.Errorf("idToken = %q, want empty", idToken)
	}
}
