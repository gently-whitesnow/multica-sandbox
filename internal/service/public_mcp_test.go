package service

import (
	"context"
	"testing"
)

func TestPublicMCPConfigurationDoesNotRequireIdentityFile(t *testing.T) {
	c := Config{OpenCode: &OpenCodeConfig{MCPPublicProxy: "http://egress:3128", MCPRelay: &RelayConfig{Listen: ":8095", URL: "http://mcp-relay:8095"}}}
	issuer, authority, err := attemptIdentity(context.Background(), c)
	if err != nil || issuer != nil || authority != nil {
		t.Fatalf("public MCP-only configuration: %v", err)
	}
	c.OpenCode.MCPRelay = nil
	if _, _, err = attemptIdentity(context.Background(), c); err == nil {
		t.Fatal("public proxy accepted without MCP relay")
	}
	c.OpenCode.MCPRelay = &RelayConfig{}
	c.OpenCode.MCPPublicProxy = "http://user:secret@egress:3128"
	if _, _, err = attemptIdentity(context.Background(), c); err == nil {
		t.Fatal("credential-bearing public proxy accepted")
	}
}
