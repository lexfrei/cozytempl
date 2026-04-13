package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTokenHandler() *Handler {
	return &Handler{
		store: NewSessionStore(testSessionKey),
		log:   slog.New(slog.DiscardHandler),
	}
}

func postTokenForm(t *testing.T, hnd *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/auth/token",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	hnd.HandleTokenUpload(rec, req)

	return rec
}

func TestHandleTokenUpload_EmptyTokenRendersForm(t *testing.T) {
	hnd := newTokenHandler()

	stub := stubTokenProbeOK(t)
	defer stub()

	rec := postTokenForm(t, hnd, url.Values{"token": {""}}.Encode())

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-render)", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), ErrTokenEmpty.Error()) {
		t.Errorf("body missing empty-token error; body = %q", rec.Body.String())
	}
}

func TestHandleTokenUpload_OversizedRejected(t *testing.T) {
	hnd := newTokenHandler()

	stub := stubTokenProbeOK(t)
	defer stub()

	huge := strings.Repeat("a", int(tokenMaxBytes)+1)
	rec := postTokenForm(t, hnd, url.Values{"token": {huge}}.Encode())

	// Either the MaxBytesReader 413 or the in-handler size check is
	// acceptable — both stop the oversized payload before it lands in
	// the session.
	if rec.Code == http.StatusSeeOther {
		t.Errorf("oversized token accepted (303 redirect); want rejection")
	}
}

func TestHandleTokenUpload_TrimsWhitespace(t *testing.T) {
	hnd := newTokenHandler()

	var capturedToken string

	probeTokenFn = func(_ context.Context, _ string) error {
		capturedToken = "captured-via-stub"

		return nil
	}
	t.Cleanup(func() { probeTokenFn = probeToken })

	rec := postTokenForm(t, hnd, url.Values{"token": {"  abc.def.ghi  \r\n"}}.Encode())

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body = %q", rec.Code, rec.Body.String())
	}

	if capturedToken == "" {
		t.Fatal("probe stub was not invoked")
	}

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set after upload")
	}

	store := hnd.store

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	for _, c := range cookies {
		req2.AddCookie(c)
	}

	session, err := store.Get(req2)
	if err != nil {
		t.Fatalf("reading session back: %v", err)
	}

	tok, ok := GetBearerToken(session)
	if !ok {
		t.Fatal("session has no bearer token after upload")
	}

	if tok != "abc.def.ghi" {
		t.Errorf("stored token = %q, want trimmed value", tok)
	}
}

func TestHandleTokenUpload_ProbeFailureReRendersForm(t *testing.T) {
	hnd := newTokenHandler()

	probeTokenFn = func(_ context.Context, _ string) error {
		return errors.New("apiserver unreachable")
	}
	t.Cleanup(func() { probeTokenFn = probeToken })

	rec := postTokenForm(t, hnd, url.Values{"token": {"abc"}}.Encode())

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-render with error)", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "apiserver unreachable") {
		t.Errorf("body missing probe error; body = %q", rec.Body.String())
	}
}

// stubTokenProbeOK swaps the package-level probe with a no-op that
// always succeeds, returning a deferred restore.
func stubTokenProbeOK(t *testing.T) func() {
	t.Helper()

	probeTokenFn = func(_ context.Context, _ string) error { return nil }

	return func() { probeTokenFn = probeToken }
}
