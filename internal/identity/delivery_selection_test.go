package identity

import "testing"

func TestManagedMCPIsAnExactIdentityRecipient(t *testing.T) {
	s := &Service{mcp: map[string]bool{"https://eco.example.invalid/mcp": true}, anon: map[string]bool{"https://docs.example.invalid/mcp": true}}
	if !s.ManagedMCP("https://eco.example.invalid/mcp") {
		t.Fatal("managed recipient not recognized")
	}
	for _, endpoint := range []string{"https://eco.example.invalid/mcp/", "https://docs.example.invalid/mcp", "https://new.example.invalid/mcp"} {
		if s.ManagedMCP(endpoint) {
			t.Fatal("identity recipient broadened")
		}
	}
}
