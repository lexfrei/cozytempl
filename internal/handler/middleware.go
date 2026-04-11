package handler

import (
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// requireUser extracts the authenticated user from the request
// context, or writes a 401 response and returns nil if the request
// is unauthenticated. Every HTML handler in this package used to
// inline the same four-line preamble; this helper collapses it to:
//
//	usr := pgh.requireUser(writer, req)
//	if usr == nil {
//		return
//	}
//
// One place to evolve the unauthorized response later (e.g. redirect
// to /auth/login on page routes, JSON on API routes) without touching
// every handler.
func (pgh *PageHandler) requireUser(writer http.ResponseWriter, req *http.Request) *auth.UserContext {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return nil
	}

	return usr
}
