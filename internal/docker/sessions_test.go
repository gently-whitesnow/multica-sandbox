//go:build containers

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
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
	// Other agents, issues, workspaces and a chat with the issue's ID get other volumes;
	// the oldest idle one goes beyond Max.
	chat := execution.Workdir{Workspace: conversation.Workspace, Agent: conversation.Agent, Chat: conversation.Issue}
	chatVolume := ""
	for i, other := range []execution.Workdir{{Workspace: conversation.Workspace, Agent: conversation.Agent, Issue: "a2000000-0000-4000-8000-000000000002"}, {Workspace: conversation.Workspace, Agent: "a1000000-0000-4000-8000-000000000002", Issue: conversation.Issue}, chat} {
		time.Sleep(10 * time.Millisecond)
		r, err := backend.Start(ctx, fmt.Sprintf("other-%d", i), other)
		if err != nil {
			t.Fatal(err)
		}
		name, reused := r.Workdir()
		if name == volume || reused || shell(r, `test ! -e note && echo empty > note && cat note`) != "empty" {
			t.Fatal("another conversation reached the retained workdir")
		}
		chatVolume = name
		if err := r.Remove(ctx); err != nil {
			t.Fatal(err)
		}
	}
	again, err := backend.Start(ctx, "chat-again", chat)
	if err != nil {
		t.Fatal(err)
	}
	if name, reused := again.Workdir(); name != chatVolume || !reused || shell(again, `cat note`) != "empty" {
		t.Fatal("a chat follow-up did not get its retained workdir")
	}
	if err := again.Remove(ctx); err != nil {
		t.Fatal(err)
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

func TestSweepRemovesSessionsOfClosedIssues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner := fmt.Sprintf("94000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	other := fmt.Sprintf("95000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	done, open := "a2000000-0000-4000-8000-000000000001", "a2000000-0000-4000-8000-000000000002"
	w1, w2 := "10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002"
	// Multica answers per workspace: the issue is done in w1 only.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"issue_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		items := []map[string]any{}
		for _, id := range body.IDs {
			category := "started"
			if id == done && r.URL.Path == "/api/daemon/workspaces/"+w1+"/issues/gc-check" {
				category = "done"
			}
			items = append(items, map[string]any{"id": id, "found": true, "category": category, "updated_at": time.Now().Add(-time.Hour)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": items})
	}))
	defer server.Close()
	api, _ := multica.New(server.URL, "fixture")
	sessions := &Sessions{Dir: t.TempDir(), TTL: time.Hour, Grace: time.Minute, Max: 8, Interval: time.Hour,
		Closed: func(ctx context.Context, workspace string, issues []string) map[string]time.Time {
			closed, _ := api.ClosedIssues(ctx, workspace, issues)
			return closed
		}}
	volumes := map[string]string{}
	for _, v := range []struct{ owner, workspace, issue string }{{owner, w1, done}, {owner, w1, open}, {owner, w2, done}, {other, w1, done}} {
		name, label := sessionName(v.owner, execution.Workdir{Workspace: v.workspace, Agent: "a1000000-0000-4000-8000-000000000001", Issue: v.issue})
		if _, err := command(ctx, "volume", "create", "--label", ownerLabel+"="+v.owner, "--label", sessionLabel+"="+label, name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = command(context.Background(), "volume", "rm", "-f", name) })
		idle := time.Now().Add(-10 * time.Minute)
		if err := os.WriteFile(filepath.Join(sessions.Dir, name), nil, 0600); err != nil || os.Chtimes(filepath.Join(sessions.Dir, name), idle, idle) != nil {
			t.Fatal(err)
		}
		volumes[v.owner+"/"+label] = name
	}
	sweepCtx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		sessions.Sweep(sweepCtx, owner, func(err error) { t.Error(err) })
		close(finished)
	}()
	closedVolume := volumes[owner+"/"+w1+"/a1000000-0000-4000-8000-000000000001/"+done]
	for {
		if _, err := command(ctx, "volume", "inspect", closedVolume); err != nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("session of a done issue survived the sweep")
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-finished
	for key, name := range volumes {
		_, err := command(ctx, "volume", "inspect", name)
		if (name == closedVolume) != (err != nil) {
			t.Errorf("%s: removed=%t", key, err != nil)
		}
	}
}
