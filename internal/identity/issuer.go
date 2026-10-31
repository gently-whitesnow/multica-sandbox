package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type issuer struct {
	config IssuerConfig
	source TokenSource
	keys   oidc.KeySet
	debug  Debug
}

func newIssuer(c IssuerConfig, source TokenSourceConfig, allowHTTP bool) (*issuer, error) {
	if c.MaxTTLSeconds == 0 {
		c.MaxTTLSeconds = 300
	}
	if c.Profile == "" {
		c.Profile = "keycloak"
	}
	if !endpoint(c.URL, allowHTTP) || !endpoint(c.JWKSURL, allowHTTP) || c.MaxTTLSeconds < 1 || c.MaxTTLSeconds > 3600 {
		return nil, ErrDenied
	}
	switch c.Profile {
	case "keycloak":
		if c.PrincipalClaim != "" && c.PrincipalClaim != "azp" {
			return nil, ErrDenied
		}
		c.PrincipalClaim = "azp"
	case "jwt":
		if !text(c.Audience, 256) || !text(c.PrincipalClaim, 128) {
			return nil, ErrDenied
		}
	default:
		return nil, ErrDenied
	}
	tokens, err := newTokenSource(source, c.TokenURL, allowHTTP)
	if err != nil {
		return nil, err
	}
	client := httpClient(c.JWKSURL)
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), c.JWKSURL)
	return &issuer{config: c, source: tokens, keys: keys}, nil
}

// issue separates credential exchange from the common signed-JWT acceptance guard.
// It never substitutes an OAuth ID token for an access token or accepts opaque tokens.
func (i *issuer) issue(ctx context.Context, c Credentials) (AccessToken, error) {
	bearer, err := i.source.Token(ctx, c)
	if err != nil {
		i.debug.printf("identity token request failed client=%s", c.ClientID)
		return AccessToken{}, ErrDenied
	}
	verifier := oidc.NewVerifier(i.config.URL, i.keys, &oidc.Config{ClientID: i.config.Audience, SkipClientIDCheck: i.config.Audience == "", SupportedSigningAlgs: []string{"RS256", "ES256"}})
	parsed, err := verifier.Verify(ctx, bearer)
	if err != nil {
		i.debug.printf("identity token rejected client=%s: signature or standard claims", c.ClientID)
		return AccessToken{}, ErrDenied
	}
	if reason := i.invalid(parsed, c); reason != "" {
		i.debug.printf("identity token rejected client=%s: %s", c.ClientID, reason)
		return AccessToken{}, ErrDenied
	}
	i.debug.printf("identity token issued client=%s expires_in=%.0fs", c.ClientID, time.Until(parsed.Expiry).Seconds())
	return AccessToken{parsed.Expiry, bearer}, nil
}

func (i *issuer) invalid(parsed *oidc.IDToken, c Credentials) string {
	claims := map[string]any{}
	if parsed.Claims(&claims) != nil {
		return "unreadable claims"
	}
	expected := c.principal
	if i.config.Profile == "keycloak" {
		expected = c.ClientID
	}
	principal, _ := claims[i.config.PrincipalClaim].(string)
	if principal != expected || expected == "" {
		return fmt.Sprintf("%s %q", i.config.PrincipalClaim, principal)
	}
	if i.config.Profile == "keycloak" && claims["typ"] != "Bearer" {
		return "typ is not Bearer"
	}
	now := time.Now()
	switch {
	case parsed.Subject == "":
		return "subject missing"
	case parsed.IssuedAt.IsZero() || parsed.IssuedAt.After(now.Add(30*time.Second)):
		return "issued-at missing or in the future"
	case !parsed.Expiry.After(now) || !parsed.Expiry.After(parsed.IssuedAt):
		return "expired"
	case parsed.Expiry.Sub(parsed.IssuedAt) > time.Duration(i.config.MaxTTLSeconds)*time.Second:
		return "lifetime exceeds max_ttl_seconds"
	}
	if raw, present := claims["nbf"]; present {
		nbf, ok := raw.(float64)
		if !ok || nbf > float64(now.Add(30*time.Second).Unix()) {
			return "not-before invalid or in the future"
		}
	}
	return ""
}
