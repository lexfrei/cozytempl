package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ErrNoIDToken is returned when the token response lacks an id_token.
var ErrNoIDToken = errors.New("no id_token in token response")

// OIDCProvider wraps the OIDC provider and OAuth2 config.
type OIDCProvider struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth2   oauth2.Config
}

// NewOIDCProvider creates an OIDC provider from the given configuration.
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
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
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

// Exchange trades an authorization code for tokens and extracts claims.
func (opr *OIDCProvider) Exchange(ctx context.Context, code string) (*Claims, string, error) {
	token, err := opr.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, "", fmt.Errorf("exchanging auth code: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, "", ErrNoIDToken
	}

	idToken, err := opr.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, "", fmt.Errorf("verifying id_token: %w", err)
	}

	var claims Claims

	claimsErr := idToken.Claims(&claims)
	if claimsErr != nil {
		return nil, "", fmt.Errorf("parsing claims: %w", claimsErr)
	}

	if claims.Username == "" {
		claims.Username = claims.Email
	}

	return &claims, rawIDToken, nil
}
