package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"itmon.nl/locutus/internal/models"
)

// OIDCClient performs the authorization code flow against one provider.
type OIDCClient struct {
	oauth2        oauth2.Config
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
}

// NewOIDCClient discovers the provider configuration and prepares a client.
func NewOIDCClient(ctx context.Context, cfg models.OIDCConfig, redirectURL string) (*OIDCClient, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", cfg.IssuerURL, err)
	}
	scopes := []string{oidc.ScopeOpenID}
	for _, s := range strings.Fields(cfg.Scopes) {
		if s != oidc.ScopeOpenID {
			scopes = append(scopes, s)
		}
	}
	claim := cfg.UsernameClaim
	if claim == "" {
		claim = "preferred_username"
	}
	return &OIDCClient{
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       scopes,
		},
		verifier:      provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		usernameClaim: claim,
	}, nil
}

// AuthURL returns the URL to redirect the browser to.
func (c *OIDCClient) AuthURL(state, nonce string) string {
	return c.oauth2.AuthCodeURL(state, oidc.Nonce(nonce))
}

// Exchange redeems the authorization code and validates the ID token.
func (c *OIDCClient) Exchange(ctx context.Context, code, nonce string) (*Identity, error) {
	token, err := c.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in token response")
	}
	idToken, err := c.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("verify id_token: %w", err)
	}
	if idToken.Nonce != nonce {
		return nil, errors.New("nonce mismatch")
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}
	str := func(key string) string {
		s, _ := claims[key].(string)
		return s
	}
	id := &Identity{
		Subject:     idToken.Subject,
		Username:    str(c.usernameClaim),
		DisplayName: str("name"),
		Email:       str("email"),
	}
	if id.Username == "" {
		id.Username = firstNonEmpty(id.Email, id.Subject)
	}
	return id, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
