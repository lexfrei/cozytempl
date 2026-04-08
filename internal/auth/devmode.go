package auth

import (
	"context"
	"net/http"
)

// DevAuth is middleware that injects a fake admin user for development.
// It skips OIDC entirely.
func DevAuth(username string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		ctx := context.WithValue(req.Context(), userContextKey, &UserContext{
			Username: username,
			Groups:   []string{"system:masters"},
		})
		next.ServeHTTP(writer, req.WithContext(ctx))
	})
}
