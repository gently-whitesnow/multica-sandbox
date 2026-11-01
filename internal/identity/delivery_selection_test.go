package identity

import (
	"context"
	"testing"
)

func TestMCPDeliveryUsesExplicitAdapterSelection(t *testing.T) {
	fixture := newTokenFixture(t)
	service, err := New(fixture.config(t), identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"https://tools.example/mcp", "https://new.example/mcp?project=one"} {
		if _, err = service.AcquireForMCP(context.Background(), Ref{identityTestServer, workspaceA, agentA}, nil, endpoint); err != nil {
			t.Fatal("selected MCP requires no infrastructure URL list", err)
		}
	}
	for _, endpoint := range []string{"", "file:///tmp/mcp", "https://user:secret@tools.example/mcp", "https://tools.example/mcp#fragment", "https:///mcp"} {
		fixture.gotSecret = ""
		if _, err = service.AcquireForMCP(context.Background(), Ref{identityTestServer, workspaceA, agentA}, nil, endpoint); err != ErrDenied || fixture.gotSecret != "" {
			t.Fatal("invalid MCP URL contacted issuer")
		}
	}
}
