package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func newTokenHandler() *Handler {
	return &Handler{
		store:   NewSessionStore(testSessionKey),
		log:     slog.New(slog.DiscardHandler),
		baseCfg: &rest.Config{Host: "https://test-apiserver.invalid"},
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

// stubTokenProbe swaps the package-level probe with fn and restores
// the original via t.Cleanup. Using Cleanup (not a returned defer)
// guarantees the global is restored even if the test fails with
// t.FailNow / FatalOpen, so neighbouring tests never observe a
// leaked stub.
func stubTokenProbe(t *testing.T, fn func(context.Context, *rest.Config, string) error) {
	t.Helper()

	probeTokenFn = fn
	t.Cleanup(func() { probeTokenFn = probeToken })
}

func TestHandleTokenUpload_EmptyTokenRendersForm(t *testing.T) {
	hnd := newTokenHandler()

	// No probe stub: the empty-token check returns before probeTokenFn
	// is ever dereferenced, so the real probe is fine here.
	rec := postTokenForm(t, hnd, url.Values{"token": {""}}.Encode())

	// Re-rendering the form with an inline error uses 200 OK on
	// purpose — the POST/redirect/GET pattern only kicks in on
	// success. Keeping 200 here matches the BYOK handler and lets
	// the form's <form> element stay the source of truth for
	// client-side state.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (intentional re-render, not 4xx)", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), ErrTokenEmpty.Error()) {
		t.Errorf("body missing empty-token error; body = %q", rec.Body.String())
	}
}

func TestHandleTokenUpload_OversizedRejected(t *testing.T) {
	hnd := newTokenHandler()

	// No probe stub needed: the size check returns before the probe.
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

	stubTokenProbe(t, func(_ context.Context, _ *rest.Config, token string) error {
		capturedToken = token

		return nil
	})

	rec := postTokenForm(t, hnd, url.Values{"token": {"  abc.def.ghi  \r\n"}}.Encode())

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body = %q", rec.Code, rec.Body.String())
	}

	if capturedToken != "abc.def.ghi" {
		t.Errorf("probe received %q, want trimmed value", capturedToken)
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

	stubTokenProbe(t, func(_ context.Context, _ *rest.Config, _ string) error {
		return errors.New("apiserver unreachable")
	})

	rec := postTokenForm(t, hnd, url.Values{"token": {"abc"}}.Encode())

	// Same POST-re-render-as-200 rationale as the empty-token case.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (intentional re-render with error)", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "apiserver unreachable") {
		t.Errorf("body missing probe error; body = %q", rec.Body.String())
	}
}

// TestHandleTokenUpload_ProbeNetworkErrorDoesNotLeakInternalAddress
// asserts that when the probe fails with a wrapped network error
// carrying an apiserver address (typical client-go shape), the
// user-facing form re-renders with the GENERIC ErrTokenUnreachable
// sentinel and NOT the wrapped detail. This is defence against
// inadvertent cluster-topology leaks on any paste-form that happens
// to be reachable outside a trusted network.
func TestHandleTokenUpload_ProbeNetworkErrorDoesNotLeakInternalAddress(t *testing.T) {
	hnd := newTokenHandler()

	const internalAddr = "10.96.0.1:6443"

	stubTokenProbe(t, func(_ context.Context, _ *rest.Config, _ string) error {
		// Shape mirrors real client-go output.
		return fmt.Errorf("%w: dial tcp %s: i/o timeout", ErrTokenUnreachable, internalAddr)
	})

	rec := postTokenForm(t, hnd, url.Values{"token": {"abc"}}.Encode())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render with generic error)", rec.Code)
	}

	if strings.Contains(rec.Body.String(), internalAddr) {
		t.Errorf("response body leaked internal address %q; body = %q", internalAddr, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), ErrTokenUnreachable.Error()) {
		t.Errorf("response body missing generic error sentinel; body = %q", rec.Body.String())
	}
}

// TestHandleTokenUpload_NearMaxSizeFitsCookie asserts that a token
// at the documented maximum size actually persists to the session
// cookie without tripping gorilla/securecookie's internal
// maxLength on the encoded value. This guards against future
// changes to tokenMaxBytes that look innocent but produce a 500 on
// first paste because the encoded payload no longer fits one cookie.
func TestHandleTokenUpload_NearMaxSizeFitsCookie(t *testing.T) {
	hnd := newTokenHandler()

	stubTokenProbe(t, func(_ context.Context, _ *rest.Config, _ string) error { return nil })

	// Use a token exactly at the cap. A single-byte over cap is
	// covered by TestHandleTokenUpload_OversizedRejected.
	bigToken := strings.Repeat("x", int(tokenMaxBytes))
	rec := postTokenForm(t, hnd, url.Values{"token": {bigToken}}.Encode())

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body = %q", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie after near-max-size upload — securecookie probably rejected the encoded value")
	}

	// Round-trip: the stored cookie must decode back to the same
	// token. If securecookie truncated or dropped the value we'd
	// read an empty string back here instead of bigToken.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	for _, c := range cookies {
		req2.AddCookie(c)
	}

	session, err := hnd.store.Get(req2)
	if err != nil {
		t.Fatalf("reading session back: %v", err)
	}

	stored, ok := GetBearerToken(session)
	if !ok {
		t.Fatal("session has no bearer token after near-max upload")
	}

	if stored != bigToken {
		t.Errorf("stored token length = %d, want %d (near-max token got truncated)", len(stored), len(bigToken))
	}
}

// TestProbeToken_NilBaseConfig asserts the probe returns the static
// sentinel when it is called without a base rest.Config. This is a
// wiring bug, not a user-actionable condition, but the sentinel lets
// main.go / tests tell the two apart instead of the probe silently
// reaching for rest.InClusterConfig() (which used to hardcode
// in-cluster detection and failed outside a cluster).
func TestProbeToken_NilBaseConfig(t *testing.T) {
	t.Parallel()

	err := probeToken(context.Background(), nil, "whatever")

	if !errors.Is(err, ErrTokenProbeMisconfigured) {
		t.Errorf("err = %v, want ErrTokenProbeMisconfigured", err)
	}
}

// TestHandleTokenUpload_SessionStoreGetFailure exercises the error
// branch in persistTokenSession when store.Get returns an error.
// A session cookie signed with a different key decrypts to an error,
// so the handler is forced down the 500 path rather than saving the
// token into a corrupt session.
func TestHandleTokenUpload_SessionStoreGetFailure(t *testing.T) {
	hnd := newTokenHandler()

	stubTokenProbe(t, func(_ context.Context, _ *rest.Config, _ string) error { return nil })

	// Build a cookie signed by a DIFFERENT store so hnd.store.Get
	// rejects it as tampered.
	otherStore := NewSessionStore("another-secret-key-32-bytes-long")

	seedReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()

	sess, err := otherStore.Get(seedReq)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	SetBearerToken(sess, "seed")

	if err := otherStore.Save(seedReq, seedRec, sess); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	tamperedCookies := seedRec.Result().Cookies()
	if len(tamperedCookies) == 0 {
		t.Fatal("expected seed cookie, got none")
	}

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/auth/token",
		strings.NewReader(url.Values{"token": {"abc.def.ghi"}}.Encode()),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for _, c := range tamperedCookies {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	hnd.HandleTokenUpload(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on session decrypt failure", rec.Code)
	}
}
