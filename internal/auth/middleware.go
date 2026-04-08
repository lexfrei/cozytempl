package auth

import (
	"context"
	"net/http"
)

type contextKey string

const userContextKey contextKey = "user"

// RequireAuth is middleware that checks for a valid session.
// Unauthenticated requests receive a 401 JSON response.
func RequireAuth(store *SessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		session, err := store.Get(req)
		if err != nil {
			http.Error(writer, `{"error":"invalid session"}`, http.StatusUnauthorized)

			return
		}

		username, groups, _ := GetUser(session)
		if username == "" {
			http.Error(writer, `{"error":"not authenticated"}`, http.StatusUnauthorized)

			return
		}

		ctx := context.WithValue(req.Context(), userContextKey, &UserContext{
			Username: username,
			Groups:   groups,
		})
		next.ServeHTTP(writer, req.WithContext(ctx))
	})
}

// UserContext holds the authenticated user's identity in request context.
type UserContext struct {
	Username string
	Groups   []string
}

// UserFromContext extracts the authenticated user from the request context.
func UserFromContext(ctx context.Context) *UserContext {
	usr, _ := ctx.Value(userContextKey).(*UserContext)

	return usr
}
