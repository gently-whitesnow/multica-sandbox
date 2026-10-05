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

type Principal struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	Subject  string `json:"subject"`
}

// Credentials cannot be serialized or formatted with their client secret.
type Credentials struct {
	Principal
	secret  string
	request TokenRequest
}

func (Credentials) Format(s fmt.State, _ rune)   { fmt.Fprint(s, "<identity credentials>") }
func (Credentials) MarshalJSON() ([]byte, error) { return nil, ErrDenied }

// AccessToken exposes its bearer only through an explicit call by a delivery adapter.
type AccessToken struct {
	ExpiresAt time.Time
	value     string
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
	Version int `json:"version"`
	Agent   Ref `json:"agent"`
	Principal
	ClientSecret string       `json:"client_secret"`
	Token        TokenRequest `json:"token,omitempty"`
}
