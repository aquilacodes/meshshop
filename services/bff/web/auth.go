package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"meshshop/internal/platform"
)

// tokenVerifier turns a bearer token into the caller's subject (user id).
type tokenVerifier interface {
	Verify(ctx context.Context, raw string) (subject string, err error)
}

// oidcVerifier validates Keycloak access tokens: RS256 signature against the
// realm's JWKS, issuer, audience, expiry, and that it is an access token.
//
// The ingress gateway already rejects bad tokens (Istio RequestAuthentication);
// the BFF checks again so a gateway misconfiguration cannot open the API.
type oidcVerifier struct{ v *oidc.IDTokenVerifier }

func newOIDCVerifier(issuer, jwksURL, audience string) (*oidcVerifier, error) {
	if issuer == "" || jwksURL == "" || audience == "" {
		return nil, errors.New("OIDC_ISSUER, OIDC_JWKS_URL and OIDC_AUDIENCE are required")
	}
	// The JWKS is fetched in-cluster (through the sidecar); the issuer is the
	// public URL that Keycloak writes into tokens.
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 5 * time.Second})
	keys := oidc.NewRemoteKeySet(ctx, jwksURL)
	return &oidcVerifier{oidc.NewVerifier(issuer, keys, &oidc.Config{
		ClientID:             audience, // checked against the aud claim
		SupportedSigningAlgs: []string{oidc.RS256},
	})}, nil
}

func (o *oidcVerifier) Verify(ctx context.Context, raw string) (string, error) {
	tok, err := o.v.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	var c struct {
		Typ string `json:"typ"`
	}
	if err := tok.Claims(&c); err != nil {
		return "", err
	}
	if c.Typ != "Bearer" { // Keycloak marks access tokens; refuse ID/refresh tokens
		return "", fmt.Errorf("token type %q is not an access token", c.Typ)
	}
	return tok.Subject, nil
}

// bearer extracts the token from "Authorization: Bearer <token>".
func bearer(r *http.Request) string {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="meshshop"`)
	platform.WriteError(w, platform.ErrUnauthorized(msg))
}
