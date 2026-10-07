package service

import (
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
)

func TestMulticaRelayRequiresCLIArtifact(t *testing.T) {
	relay := &RelayConfig{Listen: ":8091", URL: "http://multica-relay:8091"}
	for _, c := range []OpenCodeConfig{{MulticaRelay: relay}, {MulticaCLI: "example.invalid/cli@sha256:" + strings.Repeat("a", 64)}} {
		if _, err := openCodeAdapter(t.Context(), Config{OpenCode: &c}, nil, &docker.Backend{}); err == nil || !strings.Contains(err.Error(), "require each other") {
			t.Fatalf("relay and CLI artifact accepted separately: %v", err)
		}
	}
}
