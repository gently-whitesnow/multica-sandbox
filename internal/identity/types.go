// Package identity resolves controller-owned credentials and verifies issued access tokens.
// Callers supply references from trusted Multica task data, never from workload input.
package identity

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrDenied = errors.New("identity unavailable or denied")

// Ref identifies one agent binding on one Multica server and workspace.
type Ref struct {
	Server      string `json:"server"`
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
}

// Agent custom_env names of its identity (ADR 0022): the OAuth client ID of its
// service-account client and that client's secret.
const (
	EnvClientID     = "IDP_IAM_CLIENT_ID"
	EnvClientSecret = "IDP_IAM_CLIENT_SECRET"
)

// Env is a redacted view of the agent's custom_env from the claim.
type Env interface {
	Allowed(names []string) map[string]string
}

// Credentials cannot be serialized or formatted with their client secret.
type Credentials struct {
	ClientID  string
	secret    string
	request   TokenRequest
	principal string
}

func (Credentials) Format(s fmt.State, _ rune)   { fmt.Fprint(s, "<identity credentials>") }
func (Credentials) MarshalJSON() ([]byte, error) { return nil, ErrDenied }

// AccessToken exposes its bearer only through an explicit call by a delivery adapter.
type AccessToken struct {
	ExpiresAt time.Time
	value     string
}

// NewAccessToken wraps a bearer verified elsewhere, such as a test fixture's.
func NewAccessToken(bearer string, expiresAt time.Time) AccessToken {
	return AccessToken{expiresAt, bearer}
}

func (t AccessToken) Bearer() string             { return t.value }
func (AccessToken) Format(s fmt.State, _ rune)   { fmt.Fprint(s, "<access token>") }
func (AccessToken) MarshalJSON() ([]byte, error) { return nil, ErrDenied }

type Resolver interface {
	Resolve(context.Context, Ref) (Credentials, error)
}

// Request and response bind a versioned resolution exchange to the exact agent.
type resolveRequest struct {
	Version int `json:"version"`
	Agent   Ref `json:"agent"`
}
type resolveResponse struct {
	Version      int          `json:"version"`
	Agent        Ref          `json:"agent"`
	ClientID     string       `json:"client_id"`
	ClientSecret string       `json:"client_secret"`
	Token        TokenRequest `json:"token,omitempty"`
	Principal    string       `json:"principal,omitempty"`
}
