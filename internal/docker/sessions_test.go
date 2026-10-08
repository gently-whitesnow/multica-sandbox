//go:build containers

package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

func TestSessionVolumesAreRetainedPerConversation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	owner := fmt.Sprintf("93000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	template, peer := "sandbox-template-"+owner, "sandbox-peer-"+owner
	if _, err := command(ctx, "network", "create", "--internal", template); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "run", "-d", "--name", peer, "--network", template, testImage, "sleep", "300"); err != nil {
		t.Fatal(err)
	}
	sessions := &Sessions{Dir: t.TempDir(), TTL: time.Hour, Max: 2}
	backend := &Projected{Backend: Backend{Image: testImage, Owner: owner, Command: []string{"/bin/sh"}}, Network: template, Peers: []string{peer},
		Helper: helperImage(t), Forward: "endpoint:8091", Sessions: sessions}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = backend.Reconcile(ctx)
		if out, err := command(ctx, "volume", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner); err == nil {
			for _, name := range strings.Fields(string(out)) {
				_, _ = command(ctx, "volume", "rm", "-f", name)
			}
		}
		_, _ = command(ctx, "rm", "-fv", peer)
		_, _ = command(ctx, "network", "rm", template)
	})
	conversation := execution.Workdir{Workspace: "10000000-0000-4000-8000-000000000001", Agent: "a1000000-0000-4000-8000-000000000001", Issue: "a2000000-0000-4000-8000-000000000001"}
	shell := func(r execution.ProjectedRun, script string) string {
		out, err := r.Capture(ctx, []string{"/bin/sh", "-c", script}, nil)
		if err != nil {
			t.Fatalf("%s: %v %q", script, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	first, err := backend.Start(ctx, "first", conversation)
	if err != nil {
		t.Fatal(err)
	}
	volume, reused := first.Workdir()
	if !strings.HasPrefix(volume, "multica-sandbox-session-") || reused {
		t.Fatalf("new session volume: %q %t", volume, reused)
	}
	shell(first, `echo first > note && stat -c %u:%a .`)
	// A concurrent writer of the same conversation waits, then gets a per-attempt volume.
	sessionBusyWait = time.Second
	concurrent, err := backend.Start(ctx, "concurrent", conversation)
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := concurrent.Workdir(); name != "" || shell(concurrent, `test ! -e note && echo fresh`) != "fresh" {
		t.Fatal("concurrent writer shared the session volume")
	}
	if err := concurrent.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	// Restart reconciliation keeps session volumes and removes per-attempt ones.
	orphan, err := backend.Start(ctx, "orphan", execution.Workdir{})
	if err != nil {
		t.Fatal(err)
	}
	orphanVolume := orphan.(*projectedRun).volume
	if err := backend.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "volume", "inspect", orphanVolume); err == nil {
		t.Fatal("per-attempt volume survived reconciliation")
	}
	second, err := backend.Start(ctx, "second", conversation)
	if err != nil {
		t.Fatal(err)
	}
	if name, reused := second.Workdir(); name != volume || !reused || shell(second, `cat note`) != "first" {
		t.Fatal("follow-up run did not get the retained workdir")
	}
	if err := second.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	// Other agents, issues and workspaces get other volumes; the oldest idle one goes beyond Max.
	for i, other := range []execution.Workdir{{Workspace: conversation.Workspace, Agent: conversation.Agent, Issue: "a2000000-0000-4000-8000-000000000002"}, {Workspace: conversation.Workspace, Agent: "a1000000-0000-4000-8000-000000000002", Issue: conversation.Issue}} {
		time.Sleep(10 * time.Millisecond)
		r, err := backend.Start(ctx, fmt.Sprintf("other-%d", i), other)
		if err != nil {
			t.Fatal(err)
		}
		if name, reused := r.Workdir(); name == volume || reused || shell(r, `test ! -e note && echo empty`) != "empty" {
			t.Fatal("another conversation reached the retained workdir")
		}
		if err := r.Remove(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := command(ctx, "volume", "inspect", volume); err == nil {
		t.Fatal("least recently used session survived the session cap")
	}
	// Idle sessions past the TTL are collected.
	sessions.TTL = 0
	if err := sessions.Collect(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if out, _ := command(ctx, "volume", "ls", "-q", "--filter", "label="+sessionLabel, "--filter", "label="+ownerLabel+"="+owner); strings.TrimSpace(string(out)) != "" {
		t.Fatal("idle sessions survived the TTL")
	}
	if entries, _ := os.ReadDir(sessions.Dir); len(entries) != 0 {
		t.Fatal("collected sessions left records")
	}
	// A same-named volume without this conversation's labels is refused.
	name, _ := sessionName(owner, conversation)
	if _, err := command(ctx, "volume", "create", name); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Start(ctx, "forged", conversation); err == nil {
		t.Fatal("unlabelled volume accepted as a session")
	}
	if _, err := os.Stat(filepath.Join(sessions.Dir, name)); err != nil {
		t.Fatal("refused session was not released")
	}
	_, _ = command(ctx, "volume", "rm", "-f", name)
}
