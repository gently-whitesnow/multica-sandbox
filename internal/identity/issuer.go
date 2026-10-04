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
	if !text(c.Name, 128) || !endpoint(c.URL, allowHTTP) || !endpoint(c.TokenURL, allowHTTP) || !endpoint(c.JWKSURL, allowHTTP) || len(c.Resources) == 0 {
		return nil, ErrDenied
	}
	resources := map[string]Resource{}
	for name, r := range c.Resources {
		if !text(name, 128) || !text(r.Audience, 1024) || r.MaxTTLSeconds < 1 || r.MaxTTLSeconds > 3600 || (r.Resource != "" && !endpoint(r.Resource, allowHTTP)) {
			return nil, ErrDenied
		}
		for _, scope := range r.Scopes {
			if !text(scope, 256) || strings.ContainsAny(scope, " \t") {
				return nil, ErrDenied
			}
		}
		r.Scopes = append([]string(nil), r.Scopes...)
		resources[name] = r
	}
	c.Resources = resources
	client := httpClient(c.TokenURL, c.JWKSURL)
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), c.JWKSURL)
	return &issuer{c, client, keys}, nil
}

// This first adapter verifies Keycloak service-account JWTs (RS256, typ=Bearer, azp).
func (i *issuer) issue(ctx context.Context, c Credentials, name string) (AccessToken, error) {
	r, ok := i.config.Resources[name]
	if !ok {
		return AccessToken{}, ErrDenied
	}
	params := url.Values{}
	if r.Resource != "" {
		params.Set("resource", r.Resource)
	}
	request := clientcredentials.Config{ClientID: c.ClientID, ClientSecret: c.secret, TokenURL: i.config.TokenURL, Scopes: r.Scopes, EndpointParams: params, AuthStyle: oauth2.AuthStyleInParams}
	token, err := request.Token(context.WithValue(ctx, oauth2.HTTPClient, i.client))
	if err != nil || !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" {
		return AccessToken{}, ErrDenied
	}
	verifier := oidc.NewVerifier(i.config.URL, i.keys, &oidc.Config{ClientID: r.Audience, SupportedSigningAlgs: []string{"RS256"}})
	parsed, err := verifier.Verify(ctx, token.AccessToken)
	if err != nil {
		return AccessToken{}, ErrDenied
	}
	var claims struct {
		Client string `json:"azp"`
		Type   string `json:"typ"`
	}
	now := time.Now()
	if parsed.Claims(&claims) != nil || parsed.Subject != c.Subject || claims.Client != c.ClientID || claims.Type != "Bearer" || parsed.IssuedAt.IsZero() || parsed.IssuedAt.After(now.Add(30*time.Second)) || !parsed.Expiry.After(now) || !parsed.Expiry.After(parsed.IssuedAt) || parsed.Expiry.Sub(parsed.IssuedAt) > time.Duration(r.MaxTTLSeconds)*time.Second {
		return AccessToken{}, ErrDenied
	}
	return AccessToken{parsed.Expiry, token.AccessToken}, nil
}
