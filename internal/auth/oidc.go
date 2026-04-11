package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ErrNoIDToken is returned when the token response lacks an id_token.
var ErrNoIDToken = errors.New("no id_token in token response")

// ErrMalformedJWT is returned when an input JWT cannot be split into
// three base64 segments. The helper is deliberately lightweight — it
// does NOT verify the signature. Expiry is read with a split-and-decode
// so RequireAuth can cheaply decide whether to call the OIDC provider
// for a refresh without re-walking JWKS on every request.
var ErrMalformedJWT = errors.New("malformed JWT")

// OIDCProvider wraps the OIDC provider and OAuth2 config.
type OIDCProvider struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth2   oauth2.Config
}

// NewOIDCProvider creates an OIDC provider from the given configuration.
// The `offline_access` scope is requested so Keycloak issues a refresh
// token; without it, sessions would hard-expire at ID-token TTL (~15
// minutes for stock cozystack Keycloak) and force the user to re-login
// mid-task.
func NewOIDCProvider(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL string) (*OIDCProvider, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("creating OIDC provider: %w", err)
	}

	oauthCfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes: []string{
			oidc.ScopeOpenID,
			"profile",
			"email",
			"groups",
			oidc.ScopeOfflineAccess,
		},
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})

	return &OIDCProvider{
		provider: provider,
		verifier: verifier,
		oauth2:   oauthCfg,
	}, nil
}

// AuthCodeURL returns the URL to redirect the user to for authentication.
func (opr *OIDCProvider) AuthCodeURL(state string) string {
	return opr.oauth2.AuthCodeURL(state)
}

// Claims holds the OIDC token claims we extract.
// Field tags use OIDC standard claim names, not camelCase.
type Claims struct {
	Username string   `json:"preferred_username"` //nolint:tagliatelle // OIDC standard claim name
	Email    string   `json:"email"`
	Groups   []string `json:"groups"`
}

// ExchangeResult is the full set of values Exchange produces. Bundling
// them in a struct avoids a four-value return and lets callers ignore
// fields they do not need (e.g. tests that only care about the claims).
type ExchangeResult struct {
	Claims       *Claims
	IDToken      string
	RefreshToken string
}

// Exchange trades an authorization code for tokens and extracts claims.
// The returned ExchangeResult carries both the raw ID token (for
// passthrough Bearer usage) and the refresh token (for proactive
// refresh in the middleware).
func (opr *OIDCProvider) Exchange(ctx context.Context, code string) (*ExchangeResult, error) {
	token, err := opr.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging auth code: %w", err)
	}

	return opr.extractResult(ctx, token)
}

// Refresh trades a refresh token for a new access/ID/refresh token set.
// Keycloak (and most OIDC providers) may rotate the refresh token, so
// callers must persist whatever comes back — the old token may have
// been invalidated. A 400/401 from the provider surfaces as an error
// the middleware uses to clear the session and redirect to login.
func (opr *OIDCProvider) Refresh(ctx context.Context, refreshToken string) (*ExchangeResult, error) {
	tokenSource := opr.oauth2.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})

	token, err := tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("refreshing token: %w", err)
	}

	return opr.extractResult(ctx, token)
}

// extractResult pulls the id_token out of the provider response, verifies
// it, parses our slim Claims subset, and returns everything the session
// layer needs to remember.
func (opr *OIDCProvider) extractResult(ctx context.Context, token *oauth2.Token) (*ExchangeResult, error) {
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, ErrNoIDToken
	}

	idToken, err := opr.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verifying id_token: %w", err)
	}

	var claims Claims

	err = idToken.Claims(&claims)
	if err != nil {
		return nil, fmt.Errorf("parsing claims: %w", err)
	}

	if claims.Username == "" {
		claims.Username = claims.Email
	}

	return &ExchangeResult{
		Claims:       &claims,
		IDToken:      rawIDToken,
		RefreshToken: token.RefreshToken,
	}, nil
}

// jwtExpiryParts is the minimum set of claims we need to decide whether
// to refresh: just the exp timestamp. Decoding the full claims would
// cost more CPU and require more fields to stay in sync with Keycloak.
type jwtExpiryParts struct {
	Exp int64 `json:"exp"`
}

// jwtSegments is the exact number of dot-separated base64 segments a
// compact-serialised JWS has: header, payload, signature.
const jwtSegments = 3

// IDTokenExpiry parses the `exp` claim from a JWT without verifying the
// signature. This is safe for "should I refresh yet?" decisions — the
// token is verified again on the next real authenticated call (k8s API
// server in passthrough mode). Returns an error on malformed input so
// the caller can treat the session as expired.
func IDTokenExpiry(rawJWT string) (time.Time, error) {
	parts := strings.Split(rawJWT, ".")
	if len(parts) != jwtSegments {
		return time.Time{}, ErrMalformedJWT
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding jwt payload: %w", err)
	}

	var parsed jwtExpiryParts

	err = json.Unmarshal(payload, &parsed)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing jwt payload: %w", err)
	}

	if parsed.Exp == 0 {
		return time.Time{}, fmt.Errorf("%w: missing exp claim", ErrMalformedJWT)
	}

	return time.Unix(parsed.Exp, 0), nil
}
