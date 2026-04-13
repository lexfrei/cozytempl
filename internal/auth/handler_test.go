package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lexfrei/cozytempl/internal/config"
)

func TestHandleLogout_RedirectPathPerMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     config.AuthMode
		wantPath string
	}{
		{"passthrough routes to login", config.AuthModePassthrough, pathAuthLogin},
		{"impersonation-legacy routes to login", config.AuthModeImpersonationLegacy, pathAuthLogin},
		{"byok routes to kubeconfig upload", config.AuthModeBYOK, pathAuthKubeconfig},
		{"token routes to token paste", config.AuthModeToken, pathAuthToken},
		{"dev routes to root", config.AuthModeDev, "/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hnd := &Handler{
				store: NewSessionStore(testSessionKey),
				log:   testLogger(),
				mode:  tc.mode,
			}

			req := httptest.NewRequestWithContext(
				context.Background(), http.MethodPost, "/auth/logout", nil,
			)
			rec := httptest.NewRecorder()

			hnd.HandleLogout(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}

			if got := rec.Header().Get("Location"); got != tc.wantPath {
				t.Errorf("Location = %q, want %q", got, tc.wantPath)
			}
		})
	}
}
