package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// TestRateLimitAllowsBurst locks in the documented behaviour: a
// single user should be able to fire 30 requests back-to-back
// (one interactive burst like opening the tenant page) without
// getting blocked.
func TestRateLimitAllowsBurst(t *testing.T) {
	t.Parallel()

	store := newRateLimitStore()
	defer store.stop()

	handler := withRateLimit(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := range rateLimitBurst {
		req := newAuthedRequest("alice")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200 (burst should be allowed)", i, rec.Code)
		}
	}
}

// TestRateLimitBlocksExcess verifies the bucket actually stops a
// user who blasts past the burst. Without this, a cozytempl user
// could still DoS the k8s API even though we "have a rate limiter".
func TestRateLimitBlocksExcess(t *testing.T) {
	t.Parallel()

	store := newRateLimitStore()
	defer store.stop()

	handler := withRateLimit(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Drain the bucket.
	for range rateLimitBurst {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, newAuthedRequest("bob"))
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected early block during burst drain")
		}
	}

	// Next request should be blocked.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuthedRequest("bob"))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("post-burst request: got %d, want 429", rec.Code)
	}

	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q, want %q", got, "1")
	}

	if got := rec.Header().Get("Hx-Reswap"); got != "none" {
		t.Errorf("Hx-Reswap: got %q, want %q", got, "none")
	}
}

// TestRateLimitPerUserIsolation makes sure one user eating their
// bucket doesn't starve another user. The previous iteration used
// a global limiter and a noisy user blocked everyone else.
func TestRateLimitPerUserIsolation(t *testing.T) {
	t.Parallel()

	store := newRateLimitStore()
	defer store.stop()

	handler := withRateLimit(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Drain alice's bucket.
	for range rateLimitBurst {
		handler.ServeHTTP(httptest.NewRecorder(), newAuthedRequest("alice"))
	}

	// Alice should now be blocked.
	alice := httptest.NewRecorder()
	handler.ServeHTTP(alice, newAuthedRequest("alice"))
	if alice.Code != http.StatusTooManyRequests {
		t.Fatalf("alice should be blocked, got %d", alice.Code)
	}

	// Bob should still get through on a fresh bucket.
	bob := httptest.NewRecorder()
	handler.ServeHTTP(bob, newAuthedRequest("bob"))
	if bob.Code != http.StatusOK {
		t.Errorf("bob should not be affected by alice, got %d", bob.Code)
	}
}

// TestRateLimitBypassesAnonymous covers the intentional carve-out:
// unauthenticated requests (OIDC callback, static assets, /healthz)
// flow through without a limit. They're rate-limited elsewhere or
// not worth protecting.
func TestRateLimitBypassesAnonymous(t *testing.T) {
	t.Parallel()

	store := newRateLimitStore()
	defer store.stop()

	handler := withRateLimit(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 100 unauthenticated requests — way over the burst.
	for i := range 100 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("anonymous request %d blocked with %d", i, rec.Code)
		}
	}
}

// newAuthedRequest builds a request with a UserContext pinned to
// the given username. Tests use this to simulate the middleware
// chain after auth has already run.
func newAuthedRequest(username string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	ctx := auth.ContextWithUser(req.Context(), &auth.UserContext{Username: username})

	return req.WithContext(ctx)
}
