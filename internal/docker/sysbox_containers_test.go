//go:build containers

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

// registryImage is registry:3, the pull-through mirror peer of sysbox attempts.
const registryImage = "registry@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8"

// template creates an internal template network holding one peer, aliased mirror: a
// pull-through registry with an upstream network when registry is set, else a sleeper.
func template(t *testing.T, owner string, registry bool) (string, string) {
	t.Helper()
	ctx := context.Background()
	network, peer, upstream := "sandbox-template-"+owner, "sandbox-mirror-"+owner, "sandbox-upstream-"+owner
	t.Cleanup(func() {
		_, _ = command(context.Background(), "rm", "-fv", peer)
		_, _ = command(context.Background(), "network", "rm", network, upstream)
	})
	run := []string{"run", "-d", "--name", peer, "--network", network, "--network-alias", "mirror", testImage, "sleep", "600"}
	if registry {
		if _, err := command(ctx, "network", "create", upstream); err != nil {
			t.Fatal(err)
		}
		run = []string{"run", "-d", "--name", peer, "--network", upstream, "-e", "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io", "-e", "OTEL_TRACES_EXPORTER=none", registryImage}
	}
	if _, err := command(ctx, "network", "create", "--internal", network); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, run...); err != nil {
		t.Fatal(err)
	}
	if registry {
		if _, err := command(ctx, "network", "connect", "--alias", "mirror", network, peer); err != nil {
			t.Fatal(err)
		}
	}
	return network, peer
}

// engineImage builds the example image with a static Docker engine (examples/sysbox-host).
func engineImage(t *testing.T) string {
	t.Helper()
	tag := fmt.Sprintf("multica-sandbox-engine-test:%d", time.Now().UnixNano())
	build := exec.Command("docker", "build", "-q", "-t", tag, "-f", "../../examples/sysbox-host/engine.Dockerfile", "../../examples/sysbox-host")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { command(context.Background(), "rmi", "-f", tag) })
	digest, err := command(context.Background(), "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(digest))
}

func testOwner(prefix string) string {
	return fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, time.Now().UnixNano()%1000000000000)
}

// TestSysboxAttempt follows examples/sysbox-host/smoke.sh through the backend on a
// Sysbox engine (VERIFY_SYSBOX=1, for example DOCKER_CONTEXT=lima-sysbox).
func TestSysboxAttempt(t *testing.T) {
	if os.Getenv("VERIFY_SYSBOX") != "1" {
		t.Skip("set VERIFY_SYSBOX=1 with a Sysbox engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	owner := testOwner("93000000")
	network, peer := template(t, owner, true)
	b := &Projected{Backend: Backend{Image: engineImage(t), Owner: owner, Command: []string{"/bin/sh"}, Runtime: Sysbox}, Network: network, Peers: []string{peer},
		Helper: helperImage(t), Forward: "mirror:5000", Resources: SysboxResources.InPool(os.Getenv("VERIFY_SYSBOX_POOL"), 0),
		DinD: DinD{RegistryMirrors: []string{"http://mirror:5000"}, InsecureRegistries: []string{"mirror:5000"}, RyukDisabled: true}}
	if b.Resources.Pool == "" {
		b.Resources = SysboxResources
	}
	t.Cleanup(func() { _ = b.Reconcile(context.Background()) })
	if err := b.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	a, err := b.Start(ctx, "workspace-a:attempt", execution.Workdir{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attempt ready with nested engine in %s", time.Since(started).Round(10*time.Millisecond))
	host, err := EngineID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Capture(ctx, []string{"/bin/sh", "-c", `set -e
test "$(id -u)" = 65532
test "$(stat -c %a /workspace)" = 1777
test "$TESTCONTAINERS_RYUK_DISABLED" = true
test -z "${DOCKER_HOST:-}"
touch /etc/forbidden 2>/dev/null && exit 1
docker info --format '{{.ID}}'
docker pull -q hello-world >/dev/null
docker run --rm hello-world | grep -c 'Hello from Docker!'
docker run --rm --privileged alpine:3.22 cat /proc/self/uid_map | awk '{print $1, ($2 == 0 ? "identity" : "mapped")}'
docker run --rm --privileged -v /:/attempt alpine:3.22 touch /attempt/etc/forbidden 2>/dev/null && exit 1
echo done`}, nil)
	// The agent's engine is the attempt's own, never the host engine behind the controller.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || len(lines) != 4 || lines[0] == host || lines[1] != "1" || lines[2] != "0 mapped" || lines[3] != "done" {
		t.Fatalf("nested engine checks: %v %q", err, out)
	}
	if err := a.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if left, _ := command(ctx, "ps", "-aq", "--filter", "label="+ownerLabel+"="+owner); strings.TrimSpace(string(left)) != "" {
		t.Fatal("attempt survived removal")
	}

	// An engine that cannot start rejects only the task and leaves nothing behind.
	b.DinD = DinD{RegistryMirrors: []string{"not a mirror"}, ReadyTimeout: 3 * time.Second}
	_, err = b.Start(ctx, "workspace-b:attempt", execution.Workdir{})
	var rejected *execution.RejectedError
	if !errors.As(err, &rejected) || !errors.Is(err, errEngine) {
		t.Fatalf("broken engine not rejected: %v", err)
	}
	if left, _ := command(ctx, "ps", "-aq", "--filter", "label="+ownerLabel+"="+owner); strings.TrimSpace(string(left)) != "" {
		t.Fatal("rejected attempt left a container")
	}
}

// TestOutOfMemoryIsReported classifies a kernel OOM kill of the agent command.
func TestOutOfMemoryIsReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	owner := testOwner("94000000")
	network, peer := template(t, owner, false)
	limits := probeResources
	limits.Memory = 64 << 20
	b := &Projected{Backend: Backend{Image: testImage, Owner: owner, Command: []string{"/bin/sh"}}, Network: network, Peers: []string{peer}, Resources: limits}
	t.Cleanup(func() { _ = b.Reconcile(context.Background()) })
	a, err := b.Start(ctx, "workspace-a:attempt", execution.Workdir{})
	if err != nil {
		t.Fatal(err)
	}
	discard := func(_ io.Writer, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }
	if err := a.Stream(ctx, []string{"/bin/sh", "-c", "exit 3"}, nil, discard); err == nil || errors.As(err, new(*execution.OOMError)) {
		t.Fatalf("plain failure reported as out of memory: %v", err)
	}
	// The adapter reads a stream that ends without its terminal event; the kill wins.
	streamEnded := func(_ io.Writer, r io.Reader) error {
		_, _ = io.Copy(io.Discard, r)
		return errors.New("stream ended early")
	}
	for _, consume := range []func(io.Writer, io.Reader) error{discard, streamEnded} {
		err = a.Stream(ctx, []string{"/bin/sh", "-c", "tail /dev/zero"}, nil, consume)
		var oom *execution.OOMError
		if !errors.As(err, &oom) || oom.Killed < 1 || oom.Limit != 64<<20 || oom.Pool {
			t.Fatalf("OOM kill not reported: %v", err)
		}
		t.Log(oom.Error())
	}
}
