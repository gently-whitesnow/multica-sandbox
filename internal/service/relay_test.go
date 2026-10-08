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

func TestRepositoryCheckoutRequiresHelperAndRelays(t *testing.T) {
	relay := &RelayConfig{Listen: ":8093", URL: "http://git-relay:8093"}
	cli := "example.invalid/cli@sha256:" + strings.Repeat("a", 64)
	multica := &RelayConfig{Listen: ":8091", URL: "http://multica-relay:8091"}
	for name, c := range map[string]OpenCodeConfig{
		"relay only":     {GitRelay: relay, Helper: cli, MulticaRelay: multica, MulticaCLI: cli},
		"file only":      {GitFile: "/etc/git.json", Helper: cli, MulticaRelay: multica, MulticaCLI: cli},
		"no helper":      {GitRelay: relay, GitFile: "/etc/git.json", MulticaRelay: multica, MulticaCLI: cli},
		"helper only":    {Helper: cli},
		"no multica cli": {GitRelay: relay, GitFile: "/etc/git.json", Helper: cli},
	} {
		if _, err := openCodeAdapter(t.Context(), Config{OpenCode: &c}, nil, &docker.Backend{}); err == nil || !strings.Contains(err.Error(), "require") {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
