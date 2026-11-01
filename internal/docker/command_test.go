package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
}

// Leftovers of a crash under memory pressure get the attempt removal deadline.
func TestReconcileWaitsForSlowRemoval(t *testing.T) {
	defer func(timeout time.Duration) { commandTimeout = timeout }(commandTimeout)
	commandTimeout = 3 * time.Second
	fakeDocker(t, `case "$1" in
ps) echo leftover ;;
rm) sleep 4 ;;
esac
`)
	b := &Backend{Owner: "90000000-0000-4000-8000-000000000001"}
	if err := b.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Engine messages explain failures; attempt process stderr stays out.
func TestFailureKeepsEngineMessage(t *testing.T) {
	fakeDocker(t, `echo "Error response from daemon: engine detail" >&2; exit 1`)
	if _, err := command(context.Background(), "network", "inspect", "template"); err == nil || !strings.Contains(err.Error(), "engine detail") {
		t.Fatalf("engine message dropped: %v", err)
	}
	for _, args := range [][]string{{"exec", "attempt", "cat"}, {"start", "--attach", "probe"}} {
		if _, err := command(context.Background(), args...); err == nil || strings.Contains(err.Error(), "detail") {
			t.Fatalf("%s: process stderr exposed: %v", args[0], err)
		}
	}
}
