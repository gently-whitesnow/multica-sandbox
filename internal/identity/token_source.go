package identity

import (
	"context"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// TokenSource obtains a JWT using controller-owned credentials. Issuance alone is
// insufficient: every result passes the configured issuer's shared JWT validator.
type TokenSource interface {
	Token(context.Context, Credentials) (string, error)
}

type oauthSource struct {
	url  string
	auth oauth2.AuthStyle
}

func newTokenSource(c TokenSourceConfig, legacyURL string, allowHTTP bool) (TokenSource, error) {
	if c.TokenURL != "" && legacyURL != "" && c.TokenURL != legacyURL {
		return nil, ErrDenied
	}
	if c.Type == "" {
		c.Type = "oauth2_client_credentials"
	}
	if c.TokenURL == "" {
		c.TokenURL = legacyURL
	}
	if !endpoint(c.TokenURL, allowHTTP) {
		return nil, ErrDenied
	}
	switch c.Type {
	case "oauth2_client_credentials":
		if c.LoginURL != "" || c.RevokeURL != "" {
			return nil, ErrDenied
		}
		auth := oauth2.AuthStyleInParams
		switch c.AuthMethod {
		case "", "client_secret_post":
		case "client_secret_basic":
			auth = oauth2.AuthStyleInHeader
		default:
			return nil, ErrDenied
		}
		return oauthSource{c.TokenURL, auth}, nil
	case "openbao_approle":
		if !endpoint(c.LoginURL, allowHTTP) || !endpoint(c.RevokeURL, allowHTTP) || legacyURL != "" || c.AuthMethod != "" {
			return nil, ErrDenied
		}
		// Never send a temporary Bao auth token to another origin.
		login, _ := url.Parse(c.LoginURL)
		for _, address := range []string{c.TokenURL, c.RevokeURL} {
			target, _ := url.Parse(address)
			if target.Scheme != login.Scheme || target.Host != login.Host {
				return nil, ErrDenied
			}
		}
		return baoSource{c, httpClient(c.LoginURL, c.TokenURL, c.RevokeURL)}, nil
	}
	return nil, ErrDenied
}

func (s oauthSource) Token(ctx context.Context, c Credentials) (string, error) {
	params := url.Values{}
	if c.request.Resource != "" {
		params.Set("resource", c.request.Resource)
	}
	request := clientcredentials.Config{ClientID: c.ClientID, ClientSecret: c.secret, TokenURL: s.url, Scopes: c.request.Scopes, EndpointParams: params, AuthStyle: s.auth}
	token, err := request.Token(context.WithValue(ctx, oauth2.HTTPClient, httpClient(s.url)))
	if err != nil || !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" {
		return "", ErrDenied
	}
	return token.AccessToken, nil
}
