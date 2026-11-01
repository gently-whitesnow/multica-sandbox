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

// Admission inspects every image in one request per attempt and reuses validated host
// facts; failures are not cached.
func TestAdmissionReusesHostFacts(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events")
	t.Setenv("DOCKER_TEST_EVENTS", events)
	fakeDocker(t, `printf '%s\n' "$1 $2" >> "$DOCKER_TEST_EVENTS"
case "$1 $2" in
info*) test -e "$DOCKER_TEST_EVENTS.up" || { touch "$DOCKER_TEST_EVENTS.up"; exit 1; }
  printf '%s' '{"Architecture":"x86_64","OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
"image inspect") shift 4; for ref; do echo '{"Os":"linux","Architecture":"amd64"}'; done ;;
"network inspect") echo '[{"Internal":true,"Driver":"bridge","Containers":{"a":{"Name":"peer"}}}]' ;;
*) exit 2 ;;
esac
`)
	pinned := func(c string) string { return "example.invalid/" + c + "@sha256:" + strings.Repeat(c, 64) }
	b := &Projected{Backend: Backend{Owner: "90000000-0000-4000-8000-000000000001", Image: pinned("a"), Command: []string{"/bin/true"}},
		Network: "template", Peers: []string{"peer"}, Helper: pinned("b"), Forward: "relay:1",
		Bundles: []Bundle{{Image: pinned("c"), Target: "/opt/multica-sandbox/cli"}, {Image: pinned("d"), Target: "/opt/multica-sandbox/tools/jq"}}}
	if _, _, err := b.admit(context.Background()); err == nil {
		t.Fatal("failed engine check admitted")
	}
	for range 2 {
		if _, _, err := b.admit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(events)
	if want := "info --format\ninfo --format\nimage inspect\nnetwork inspect\nimage inspect\n"; string(data) != want {
		t.Fatalf("calls=%q, want %q", data, want)
	}
}
