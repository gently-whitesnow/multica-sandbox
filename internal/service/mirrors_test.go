package service

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
)

func TestReferenceMirrorsConfig(t *testing.T) {
	state := t.TempDir()
	if _, err := referenceMirrors(MirrorsConfig{}, state, &docker.Projected{}, nil, slog.Default()); err == nil {
		t.Fatal("mirrors accepted without the session sweep")
	}
	disk := pool.Disk{Total: 100 << 30, Available: 50 << 30}
	workloads := &docker.Projected{Sessions: &docker.Sessions{Disk: func() (pool.Disk, error) { return disk, nil }}}
	m, err := referenceMirrors(MirrorsConfig{}, state, workloads, nil, slog.Default())
	if err != nil || m.Refresh != 15*time.Minute || m.Wait != 2*time.Minute || m.Timeout != 10*time.Minute || m.TTL != 720*time.Hour || m.Workloads != workloads || workloads.Sessions.Mirrors != m {
		t.Fatalf("defaults %+v %v", m, err)
	}
	if info, err := os.Stat(filepath.Join(state, "mirrors")); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("mirror records directory")
	}
	// New mirrors wait for the claim pause watermark to clear.
	if m.Low() {
		t.Fatal("ample disk reported low")
	}
	disk.Available = 1 << 30
	if !m.Low() {
		t.Fatal("low disk not reported")
	}
	for name, c := range map[string]MirrorsConfig{
		"wait at checkout limit": {Wait: "5m"},
		"timeout below wait":     {Wait: "2m", Timeout: "1m"},
		"ttl within timeout":     {TTL: "10m"},
		"negative refresh":       {Refresh: "-1m"},
		"malformed ttl":          {TTL: "monthly"},
	} {
		if _, err := referenceMirrors(c, state, workloads, nil, slog.Default()); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
