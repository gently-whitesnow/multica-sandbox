package identity

import (
	"context"
	"errors"
	"fmt"
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
	debug  Debug
}

func newIssuer(c IssuerConfig, allowHTTP bool) (*issuer, error) {
	if c.MaxTTLSeconds == 0 {
		c.MaxTTLSeconds = 300
	}
	if !endpoint(c.URL, allowHTTP) || !endpoint(c.TokenURL, allowHTTP) || !endpoint(c.JWKSURL, allowHTTP) || c.MaxTTLSeconds < 1 || c.MaxTTLSeconds > 3600 {
		return nil, ErrDenied
	}
	client := httpClient(c.TokenURL, c.JWKSURL)
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), c.JWKSURL)
	return &issuer{config: c, client: client, keys: keys}, nil
}

// This first adapter verifies Keycloak service-account JWTs: RS256 signature of the
// configured realm, typ=Bearer, azp of the client and a bounded lifetime.
func (i *issuer) issue(ctx context.Context, c Credentials) (AccessToken, error) {
	params := url.Values{}
	if c.request.Resource != "" {
		params.Set("resource", c.request.Resource)
	}
	request := clientcredentials.Config{ClientID: c.ClientID, ClientSecret: c.secret, TokenURL: i.config.TokenURL, Scopes: c.request.Scopes, EndpointParams: params, AuthStyle: oauth2.AuthStyleInParams}
	token, err := request.Token(context.WithValue(ctx, oauth2.HTTPClient, i.client))
	if err != nil {
		// The error code (invalid_client, unauthorized_client) names the cause; the body is withheld.
		var refused *oauth2.RetrieveError
		if errors.As(err, &refused) && refused.Response != nil {
			i.debug.printf("identity token request refused client=%s status=%d error=%s", c.ClientID, refused.Response.StatusCode, refused.ErrorCode)
		} else {
			i.debug.printf("identity token request failed client=%s", c.ClientID)
		}
		return AccessToken{}, ErrDenied
	}
	if !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" {
		i.debug.printf("identity token response client=%s has token_type %q", c.ClientID, token.TokenType)
		return AccessToken{}, ErrDenied
	}
	// MCP validates audience and roles; issuance verifies the token is the client's.
	verifier := oidc.NewVerifier(i.config.URL, i.keys, &oidc.Config{SkipClientIDCheck: true, SupportedSigningAlgs: []string{"RS256"}})
	parsed, err := verifier.Verify(ctx, token.AccessToken)
	if err != nil {
		i.debug.printf("identity token rejected client=%s: %v", c.ClientID, err)
		return AccessToken{}, ErrDenied
	}
	if reason := i.invalid(parsed, c.ClientID); reason != "" {
		i.debug.printf("identity token rejected client=%s: %s", c.ClientID, reason)
		return AccessToken{}, ErrDenied
	}
	i.debug.printf("identity token issued client=%s expires_in=%.0fs", c.ClientID, time.Until(parsed.Expiry).Seconds())
	return AccessToken{parsed.Expiry, token.AccessToken}, nil
}

// invalid names why a verified token is not the client's bounded access token, or "".
func (i *issuer) invalid(parsed *oidc.IDToken, client string) string {
	var claims struct {
		Client string `json:"azp"`
		Type   string `json:"typ"`
	}
	now := time.Now()
	switch {
	case parsed.Claims(&claims) != nil:
		return "unreadable claims"
	case claims.Client != client:
		return fmt.Sprintf("azp %q", claims.Client)
	case claims.Type != "Bearer":
		return fmt.Sprintf("typ %q", claims.Type)
	case parsed.IssuedAt.IsZero() || parsed.IssuedAt.After(now.Add(30*time.Second)):
		return "issued-at missing or in the future"
	case !parsed.Expiry.After(now) || !parsed.Expiry.After(parsed.IssuedAt):
		return "expired"
	case parsed.Expiry.Sub(parsed.IssuedAt) > time.Duration(i.config.MaxTTLSeconds)*time.Second:
		return fmt.Sprintf("lifetime %s exceeds max_ttl_seconds %d", parsed.Expiry.Sub(parsed.IssuedAt), i.config.MaxTTLSeconds)
	}
	return ""
}
