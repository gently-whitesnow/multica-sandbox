package service

import (
	"io"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
)

func TestMulticaRelayRequiresCLIArtifact(t *testing.T) {
	relay := &RelayConfig{Listen: ":8091", URL: "http://multica-relay:8091"}
	for _, c := range []OpenCodeConfig{{MulticaRelay: relay}, {MulticaCLI: "example.invalid/cli@sha256:" + strings.Repeat("a", 64)}} {
		if _, err := openCodeAdapter(t.Context(), Config{OpenCode: &c}, t.TempDir(), nil, &docker.Backend{}, io.Discard); err == nil || !strings.Contains(err.Error(), "require each other") {
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
		"forge only":     {ForgeRelay: &RelayConfig{Listen: ":8094", URL: "https://git-relay:8094"}, Helper: cli, MulticaRelay: multica, MulticaCLI: cli},
		"https relay":    {GitRelay: &RelayConfig{Listen: ":8093", URL: "https://git-relay:8093"}, GitFile: "/etc/git.json", Helper: cli, MulticaRelay: multica, MulticaCLI: cli},
	} {
		if _, err := openCodeAdapter(t.Context(), Config{OpenCode: &c}, t.TempDir(), nil, &docker.Backend{}, io.Discard); err == nil || !strings.Contains(err.Error(), "require") {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestSessionsRequireHelperAndBounds(t *testing.T) {
	helper := "example.invalid/helper@sha256:" + strings.Repeat("a", 64)
	for name, c := range map[string]OpenCodeConfig{
		"no helper": {Sessions: &SessionsConfig{TTL: "72h", Max: 8}},
		"no ttl":    {Helper: helper, Sessions: &SessionsConfig{Max: 8}},
		"no max":    {Helper: helper, Sessions: &SessionsConfig{TTL: "72h"}},
		"huge max":  {Helper: helper, Sessions: &SessionsConfig{TTL: "72h", Max: 4096}},
		"interval":  {Helper: helper, Sessions: &SessionsConfig{TTL: "72h", Max: 8, Interval: "0s"}},
		"grace":     {Helper: helper, Sessions: &SessionsConfig{TTL: "72h", Max: 8, Grace: "soon"}},
	} {
		if _, err := openCodeAdapter(t.Context(), Config{OpenCode: &c}, t.TempDir(), nil, &docker.Backend{}, io.Discard); err == nil || !strings.Contains(err.Error(), "sessions require") {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
