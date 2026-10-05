//go:build upstream

package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

type fleetLog struct {
	sync.Mutex
	text strings.Builder
}

func (l *fleetLog) Write(p []byte) (int, error) { l.Lock(); defer l.Unlock(); return l.text.Write(p) }
func (l *fleetLog) contains(s string) bool {
	l.Lock()
	defer l.Unlock()
	return strings.Contains(l.text.String(), s)
}
func (l *fleetLog) count(s string) int {
	l.Lock()
	defer l.Unlock()
	return strings.Count(l.text.String(), s)
}
func fleetWorkspace(n int) string { return fmt.Sprintf("50000000-0000-4000-8000-%012d", n) }

func seedFleet(t *testing.T) string {
	var query strings.Builder
	for n := 1; n <= 100; n++ {
		ws := fleetWorkspace(n)
		fmt.Fprintf(&query, "INSERT INTO workspace(id,name,slug) VALUES('%s','Fleet fixture','fleet-%d');", ws, n)
		if n < 100 {
			fmt.Fprintf(&query, "INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','owner');", ws, user)
		}
	}
	pat := "mul_disposable_fleet_contract_token"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(pat)))
	fmt.Fprintf(&query, "INSERT INTO personal_access_token(user_id,name,token_hash,token_prefix) VALUES('%s','Fleet fixture','%s','mul_fixture');", user, hash)
	sql(t, query.String())
	return pat
}

func multiWorkspace(t *testing.T) {
	if os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("set VERIFY_SERVICE=1")
	}
	pat := seedFleet(t)
	upstream, _ := url.Parse(os.Getenv("MULTICA_TEST_URL"))
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var requests, claims atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/daemon/tasks/claim" {
			claims.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(pat), 0600); err != nil {
		t.Fatal(err)
	}
	c := service.Config{Server: server.URL, Concurrency: 2, Daemon: daemon, Image: image, Command: []string{"/bin/sh", "-c", "sleep 12"}, Timeout: "30s"}
	logs := &fleetLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, c, dir, tokenPath, logs) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	eventually(t, "100 workspaces registered", func() bool { return logs.contains("ready workspaces=100") })
	startRequests, startClaims := requests.Load(), claims.Load()
	started := time.Now()
	time.Sleep(16 * time.Second)
	t.Logf("100 idle workspaces: %d HTTP requests, %d batch claims in %s", requests.Load()-startRequests, claims.Load()-startClaims, time.Since(started).Round(time.Millisecond))
	if claims.Load()-startClaims > 20 {
		t.Fatal("idle claim fanout")
	}
	if sql(t, fmt.Sprintf("SELECT count(*) FROM agent_runtime WHERE workspace_id='%s';", fleetWorkspace(100))) != "0" {
		t.Fatal("registered inaccessible workspace")
	}
	api, err := multica.New(server.URL, pat)
	if err != nil {
		t.Fatal(err)
	}
	runtime := func(n int) multica.Runtime {
		return multica.Runtime{ID: sql(t, fmt.Sprintf("SELECT id FROM agent_runtime WHERE workspace_id='%s' AND daemon_id='%s';", fleetWorkspace(n), daemon))}
	}
	claimStarted := time.Now()
	first := enqueueWorkspace(t, runtime(1), 101, fleetWorkspace(1))
	second := enqueueWorkspace(t, runtime(2), 102, fleetWorkspace(2))
	eventually(t, "two workspace attempts started", func() bool { return logs.contains("started task="+first) && logs.contains("started task="+second) })
	t.Logf("two tasks started across 100 workspaces in %s", time.Since(claimStarted).Round(time.Millisecond))
	third := enqueueWorkspace(t, runtime(3), 103, fleetWorkspace(3))
	status(t, api, third, "queued")
	if len(strings.Fields(ownedContainers(t))) != 2 {
		t.Fatal("shared capacity not exercised")
	}
	fleetMembershipChange(t, api, logs, first, second)
	waitTask(t, api, third, "completed")
	if logs.count("ready workspaces=") != 1 {
		t.Fatal("unexpected restart")
	}
}
