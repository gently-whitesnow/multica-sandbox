//go:build containers

package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/repo"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

// helperImage builds the sandbox-helper target offline from the local module.
func helperImage(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	arch, err := command(ctx, "info", "--format", "{{.Architecture}}")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "sandbox-helper"), "../../cmd/sandbox-helper")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+engineArchitectures[strings.TrimSpace(string(arch))])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY --chmod=0555 sandbox-helper /sandbox-helper\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("multica-sandbox-helper-test:%d", time.Now().UnixNano())
	if _, err := command(ctx, "build", "-q", "-t", tag, dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command(context.Background(), "rmi", "-f", tag) })
	digest, err := command(ctx, "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(digest))
}

func TestWorkdirVolumeAndForwarder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	owner := fmt.Sprintf("92000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	template, peer := "sandbox-template-"+owner, "sandbox-peer-"+owner
	if _, err := command(ctx, "network", "create", "--internal", template); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "run", "-d", "--name", peer, "--network", template, "--network-alias", "endpoint", testImage, "sh", "-c",
		`while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\ncheckout' | nc -l -p 8091; done`); err != nil {
		t.Fatal(err)
	}
	backend := &Projected{Backend: Backend{Image: testImage, Owner: owner, Command: []string{"/bin/sh"}}, Network: template, Peers: []string{peer},
		Helper: helperImage(t), Forward: "endpoint:8091", ForgeHosts: []string{"api.github.com"}, ForgeForward: "endpoint:8091"}
	t.Cleanup(func() {
		_ = backend.Reconcile(context.Background())
		_, _ = command(context.Background(), "rm", "-fv", peer)
		_, _ = command(context.Background(), "network", "rm", template)
	})
	a, err := backend.Start(ctx, "workspace-a:attempt", execution.Workdir{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Capture(ctx, []string{"/bin/sh", "-c", `set -e
test "$(pwd)" = /workspace/work
test "$(stat -c %u:%g:%a /workspace/work)" = 65532:65532:700
echo kept > note
test ! -e /opt/multica-sandbox/helper/bin
case ":$PATH:" in *:/opt/multica-sandbox/helper*) exit 3 ;; esac
for p in /proc/[0-9]*; do case "$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)" in /opt/multica-sandbox/helper/sandbox-helper\ forward*) awk '/^Uid:/ {print $2}' "$p/status" ;; esac; done
wget -q -T 5 -O - http://127.0.0.1:` + repo.DaemonPort + `/repo/checkout
echo
# The peer restarts nc per connection; forge names resolve to the loopback forwarder.
for i in 1 2 3 4 5 6; do wget -q -T 5 -O - http://api.github.com:` + repo.ForgePort + `/ && break; sleep 0.5; done`}, nil)
	if err != nil || string(out) != "65532\ncheckout\ncheckout" {
		t.Fatalf("workdir volume or forwarder: %v %q", err, out)
	}
	volume := a.(*projectedRun).volume
	if got, _ := command(ctx, "volume", "inspect", "--format", `{{index .Labels "io.multica-sandbox.owner"}}`, volume); strings.TrimSpace(string(got)) != owner {
		t.Fatal("workdir volume is not owned by the controller")
	}
	if err := a.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "volume", "inspect", volume); err == nil {
		t.Fatal("workdir volume survived cleanup")
	}
	if _, err := backend.Start(ctx, "workspace-b:attempt", execution.Workdir{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := command(ctx, "volume", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner); strings.TrimSpace(string(got)) != "" {
		t.Fatal("owned workdir volume survived reconciliation")
	}
}
