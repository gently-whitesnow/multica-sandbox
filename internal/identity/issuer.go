package identity

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type issuer struct {
	config IssuerConfig
	client *http.Client
	keys   oidc.KeySet
}

func newIssuer(c IssuerConfig, allowHTTP bool) (*issuer, error) {
	if c.MaxTTLSeconds == 0 {
		c.MaxTTLSeconds = 300
	}
	if !text(c.Name, 128) || !endpoint(c.URL, allowHTTP) || !endpoint(c.TokenURL, allowHTTP) || !endpoint(c.JWKSURL, allowHTTP) || c.MaxTTLSeconds < 1 || c.MaxTTLSeconds > 3600 {
		return nil, ErrDenied
	}
	client := httpClient(c.TokenURL, c.JWKSURL)
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), c.JWKSURL)
	return &issuer{c, client, keys}, nil
}

// This first adapter verifies Keycloak service-account JWTs (RS256, typ=Bearer, azp).
func (i *issuer) issue(ctx context.Context, c Credentials) (AccessToken, error) {
	params := url.Values{}
	if c.request.Resource != "" {
		params.Set("resource", c.request.Resource)
	}
	request := clientcredentials.Config{ClientID: c.ClientID, ClientSecret: c.secret, TokenURL: i.config.TokenURL, Scopes: c.request.Scopes, EndpointParams: params, AuthStyle: oauth2.AuthStyleInParams}
	token, err := request.Token(context.WithValue(ctx, oauth2.HTTPClient, i.client))
	if err != nil || !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" {
		return AccessToken{}, ErrDenied
	}
	// MCP validates audience and roles; issuance verifies the bound principal.
	verifier := oidc.NewVerifier(i.config.URL, i.keys, &oidc.Config{SkipClientIDCheck: true, SupportedSigningAlgs: []string{"RS256"}})
	parsed, err := verifier.Verify(ctx, token.AccessToken)
	if err != nil {
		return AccessToken{}, ErrDenied
	}
	var claims struct {
		Client string `json:"azp"`
		Type   string `json:"typ"`
	}
	now := time.Now()
	if parsed.Claims(&claims) != nil || parsed.Subject != c.Subject || claims.Client != c.ClientID || claims.Type != "Bearer" || parsed.IssuedAt.IsZero() || parsed.IssuedAt.After(now.Add(30*time.Second)) || !parsed.Expiry.After(now) || !parsed.Expiry.After(parsed.IssuedAt) || parsed.Expiry.Sub(parsed.IssuedAt) > time.Duration(i.config.MaxTTLSeconds)*time.Second {
		return AccessToken{}, ErrDenied
	}
	return AccessToken{parsed.Expiry, token.AccessToken}, nil
}
