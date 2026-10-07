//go:build containers

package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

func testBackend(t *testing.T, script string) *Backend {
	t.Helper()
	b := &Backend{Image: testImage, Owner: fmt.Sprintf("90000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000), Command: []string{"/bin/sh", "-c", script}}
	t.Cleanup(func() {
		if err := b.Reconcile(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b
}
func TestOfflineBoundary(t *testing.T) {
	t.Setenv("MULTICA_PROBE_TOKEN", "host-secret-sentinel")
	b := testBackend(t, `set -eu
 test "$(id -u)" = 65532
 test "${MULTICA_PROBE_TOKEN-unset}" = unset
 test ! -e /var/run/docker.sock
 test ! -e /run/secrets
 test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
 test "$(ip -o link show up | wc -l)" = 1
 ! ip route | grep default
 ! ip -6 route | grep default
 ! wget -T 1 -q -O /tmp/leak http://169.254.169.254/
 ! wget -T 1 -q -O /tmp/leak http://1.1.1.1/
 ! timeout 2 nslookup example.com
 ! wget -T 1 -q -O /tmp/leak http://[2606:4700:4700::1111]/
 ! touch /forbidden
 ! mount -t tmpfs tmpfs /workspace
 test ! -e /workspace/other-attempt
 echo private > /workspace/other-attempt
 cp /bin/busybox /workspace/busybox && /workspace/busybox true
 cp /bin/busybox /tmp/busybox && ! /tmp/busybox true
 grep -q '^NoNewPrivs:[[:space:]]*1' /proc/self/status
 grep -q '^CapEff:[[:space:]]*0000000000000000' /proc/self/status
 grep -q '^Seccomp:[[:space:]]*2' /proc/self/status
 test "$(cat /sys/fs/cgroup/pids.max)" = 64
 test "$(cat /sys/fs/cgroup/memory.max)" = 134217728
 test "$(cat /sys/fs/cgroup/memory.swap.max)" = 0
 test "$(cat /sys/fs/cgroup/cpu.max)" = '50000 100000'
 ! dd if=/dev/zero of=/workspace/full bs=1M count=70
 `)
	lines := strings.Split(b.Command[2], "\n")
	var diagnostic strings.Builder
	for n, line := range lines {
		fmt.Fprintf(&diagnostic, "trap 'exit %d' EXIT\n%s\n", n+1, line)
	}
	diagnostic.WriteString("trap - EXIT\n")
	b.Command[2] = diagnostic.String()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Keep the first container alive in Docker while the second checks fresh state.
	for _, attempt := range []string{"first", "second"} {
		r, err := b.Start(ctx, attempt)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := command(ctx, "ps", "-aq", "--filter", "label="+ownerLabel+"="+b.Owner)
	if err != nil || len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("leaked containers: %s %v", data, err)
	}
}
func TestReconcileOnlyOwnedContainers(t *testing.T) {
	a := testBackend(t, "sleep 60")
	b := testBackend(t, "sleep 60")
	ctx := context.Background()
	first, err := a.Start(ctx, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Start(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = command(ctx, "inspect", first.(*run).name); err == nil {
		t.Fatal("orphan survived")
	}
	if _, err = command(ctx, "inspect", second.(*run).name); err != nil {
		t.Fatal("other owner removed")
	}
}

func TestResourceExhaustion(t *testing.T) {
	for name, script := range map[string]string{
		"memory":    `awk 'BEGIN { for (i=0;;i++) a[i]=sprintf("%0100000d",i) }'`,
		"processes": `set -e; for i in $(seq 1 100); do sleep 60 & done; wait`,
	} {
		t.Run(name, func(t *testing.T) {
			b := testBackend(t, script)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, err := b.Start(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			err = r.Wait(ctx)
			if ctx.Err() != nil {
				t.Fatal("resource limit did not stop execution in time")
			}
			if err == nil {
				t.Fatal("resource exhaustion unexpectedly succeeded")
			}
			if err = r.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
