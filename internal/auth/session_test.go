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

	SetUser(session, &UserSession{
		Username:     testUsername,
		Groups:       []string{"tenant-root-admin"},
		IDToken:      "fake-id-token",
		RefreshToken: "fake-refresh-token",
	})

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

	info := GetUser(session2)

	if info.Username != testUsername {
		t.Errorf("username = %q, want %q", info.Username, testUsername)
	}

	if len(info.Groups) != 1 || info.Groups[0] != "tenant-root-admin" {
		t.Errorf("groups = %v, want [tenant-root-admin]", info.Groups)
	}

	if info.IDToken != "fake-id-token" {
		t.Errorf("idToken = %q, want %q", info.IDToken, "fake-id-token")
	}

	if info.RefreshToken != "fake-refresh-token" {
		t.Errorf("refreshToken = %q, want %q", info.RefreshToken, "fake-refresh-token")
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

	SetUser(session, &UserSession{
		Username: testUsername,
		Groups:   []string{"group1"},
		IDToken:  "token",
	})
	Clear(session)

	info := GetUser(session)

	if info.Username != "" {
		t.Errorf("username after clear = %q, want empty", info.Username)
	}

	if info.Groups != nil {
		t.Errorf("groups after clear = %v, want nil", info.Groups)
	}

	if info.IDToken != "" {
		t.Errorf("idToken after clear = %q, want empty", info.IDToken)
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

	info := GetUser(session)

	if info.Username != "" {
		t.Errorf("username = %q, want empty", info.Username)
	}

	if info.Groups != nil {
		t.Errorf("groups = %v, want nil", info.Groups)
	}

	if info.IDToken != "" {
		t.Errorf("idToken = %q, want empty", info.IDToken)
	}
}
