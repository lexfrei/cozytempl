// Package auth provides OIDC authentication and session management.
package auth

import (
	"encoding/gob"
	"fmt"
	"net/http"

	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
)

const (
	sessionName      = "cozytempl"
	sessionKeyToken  = "id_token"
	sessionKeyUser   = "username"
	sessionKeyGroups = "groups"

	aes256KeySize     = 32
	sessionMaxAgeSecs = 86400
)

// SessionStore wraps gorilla/sessions for encrypted cookie storage.
type SessionStore struct {
	store *sessions.CookieStore
}

// NewSessionStore creates a session store with the given secret key.
func NewSessionStore(secret string) *SessionStore {
	// Register []string so gorilla/sessions can serialize it via gob.
	gob.Register([]string{})

	hashKey := securecookie.GenerateRandomKey(aes256KeySize)
	blockKey := padOrTruncate([]byte(secret), aes256KeySize)

	store := sessions.NewCookieStore(hashKey, blockKey)
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   sessionMaxAgeSecs,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}

	return &SessionStore{store: store}
}

func padOrTruncate(key []byte, size int) []byte {
	if len(key) >= size {
		return key[:size]
	}

	padded := make([]byte, size)
	copy(padded, key)

	return padded
}

// Get returns the session for the given request.
func (sst *SessionStore) Get(req *http.Request) (*sessions.Session, error) {
	session, err := sst.store.Get(req, sessionName)
	if err != nil {
		return nil, fmt.Errorf("getting session: %w", err)
	}

	return session, nil
}

// Save persists the session.
func (sst *SessionStore) Save(
	req *http.Request,
	writer http.ResponseWriter,
	session *sessions.Session,
) error {
	err := sst.store.Save(req, writer, session)
	if err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	return nil
}

// SetUser stores the user info in the session.
func SetUser(session *sessions.Session, username string, groups []string, idToken string) {
	session.Values[sessionKeyUser] = username
	session.Values[sessionKeyGroups] = groups
	session.Values[sessionKeyToken] = idToken
}

// GetUser retrieves the user info from the session.
// Returns empty strings/nil if not authenticated.
//
//nolint:gocritic // unnamedResult conflicts with nonamedreturns linter
func GetUser(session *sessions.Session) (string, []string, string) {
	var (
		username string
		groups   []string
		idToken  string
	)

	if val, ok := session.Values[sessionKeyUser].(string); ok {
		username = val
	}

	if val, ok := session.Values[sessionKeyGroups].([]string); ok {
		groups = val
	}

	if val, ok := session.Values[sessionKeyToken].(string); ok {
		idToken = val
	}

	return username, groups, idToken
}

// Clear removes all user data from the session.
func Clear(session *sessions.Session) {
	delete(session.Values, sessionKeyUser)
	delete(session.Values, sessionKeyGroups)
	delete(session.Values, sessionKeyToken)
	session.Options.MaxAge = -1
}
