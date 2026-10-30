//go:build containers

package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func TestProjectedAttemptIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner := fmt.Sprintf("91000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	template, peer := "sandbox-template-"+owner, "sandbox-peer-"+owner
	invoke := func(args ...string) string {
		t.Helper()
		data, err := command(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	invoke("network", "create", "--internal", template)
	invoke("run", "-d", "--name", peer, "--network", template, "--network-alias", "gateway", testImage, "sleep", "600")
	backend := &Projected{Backend: Backend{Image: testImage, Owner: owner, Command: []string{"/bin/sh"}}, Network: template, Peers: []string{peer},
		Bundles:   []Bundle{{Image: cliBundle(t, pinnedCLI), Target: "/opt/multica-sandbox/multica"}},
		Resources: Resources{NanoCPUs: 15e8, Memory: 512 << 20, Pids: 128, NoFile: 1024, Workspace: 128 << 20, Tmp: 32 << 20, Shm: 16 << 20}}
	t.Cleanup(func() {
		_ = backend.Reconcile(context.Background())
		_, _ = command(context.Background(), "rm", "-fv", peer)
		_, _ = command(context.Background(), "network", "rm", template)
	})
	a, err := backend.Start(ctx, "workspace-a:attempt", execution.Workdir{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := backend.Start(ctx, "workspace-b:attempt", execution.Workdir{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Write(ctx, "/workspace/opencode.json", []byte(`{"a":"private-a"}`)); err != nil {
		t.Fatal(err)
	}
	// Exec sessions inherit the controller PATH; the mounted CLI stays read-only.
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `set -e; test "$(command -v multica)" = /opt/multica-sandbox/multica/bin/multica; multica >/dev/null; ! touch /opt/multica-sandbox/multica/bin/x`}); err != nil {
		t.Fatal("controller CLI is not the read-only PATH entry:", err)
	}
	// `! cmd` is exempt from set -e, so denials are written `cmd && exit 1`.
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `set -e; test ! -e /workspace/opencode.json; test ! -e /var/run/docker.sock; test ! -e /run/secrets
wget -T 1 -q -O /tmp/leak http://169.254.169.254/ && exit 1; wget -T 1 -q -O /tmp/leak http://1.1.1.1/ && exit 1; :`}); err != nil {
		t.Fatal(err)
	}
	// Controls: the daemon resolves external names and the host listener answers through
	// an ordinary bridge gateway, so the denials below are not vacuous.
	listener := "sandbox-host-listener-" + owner
	t.Cleanup(func() { _, _ = command(context.Background(), "rm", "-f", listener) })
	invoke("run", "-d", "--name", listener, "--network", "host", testImage, "/bin/sh", "-c", `while true; do echo reached | nc -l -p 15353; done`)
	if _, err := command(ctx, "run", "--rm", testImage, "/bin/sh", "-c", `set -e; timeout 3 nslookup example.com >/dev/null
gw=$(ip route | awk '/default/ {print $3}'); for i in 1 2 3 4 5; do nc -w 2 "$gw" 15353 </dev/null | grep -q reached && exit 0; sleep 1; done; exit 1`); err != nil {
		t.Fatal("control container cannot resolve example.com or reach the host listener:", err)
	}
	// A hostile attempt resolves only approved peer aliases: no route, external DNS through
	// the embedded or a direct resolver, host address or IPv6.
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `set -e; nslookup gateway >/dev/null
test "$(grep '^nameserver' /etc/resolv.conf)" = "nameserver 127.0.0.11"
ip route | grep -q default && exit 1; ip -6 route | grep -q default && exit 1; test -z "$(ip -6 addr show scope global)"
timeout 3 nslookup example.com && exit 1; timeout 3 nslookup -type=TXT probe.example.com && exit 1
for dns in 1.1.1.1 8.8.8.8 2606:4700:4700::1111; do timeout 3 nslookup example.com "$dns" && exit 1; nc -w 2 "$dns" 53 </dev/null && exit 1; done
net=$(ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1 | cut -d. -f1-3)
for host in "$net.1" "$net.254"; do nc -w 2 "$host" 15353 </dev/null | grep -q reached && exit 1; done
wget -T 1 -q -O /tmp/leak http://[2606:4700:4700::1111]/ && exit 1; :`}); err != nil {
		t.Fatal("attempt network allows egress beyond approved peers:", err)
	}
	// Configured limits apply; TMPDIR is executable while /tmp stays noexec.
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `set -e; test "$(cat /sys/fs/cgroup/pids.max)" = 128; test "$(cat /sys/fs/cgroup/memory.max)" = 536870912
test "$(cat /sys/fs/cgroup/cpu.max)" = "150000 100000"; test "$(ulimit -n)" = 1024; test "$TMPDIR" = /workspace/tmp
printf '#!/bin/sh\necho ok\n' > "$TMPDIR/run"; chmod +x "$TMPDIR/run"; test "$("$TMPDIR/run")" = ok
cp "$TMPDIR/run" /tmp/run; ! /tmp/run; df -k /workspace | awk 'NR==2 { exit !($2 == 131072) }'; df -k /dev/shm | awk 'NR==2 { exit !($2 == 16384) }'`}); err != nil {
		t.Fatal("configured resources or executable TMPDIR not applied:", err)
	}
	rA, rB := a.(*projectedRun), b.(*projectedRun)
	if rA.network == rB.network {
		t.Fatal("shared attempt network")
	}
	if got := invoke("network", "inspect", "--format", "{{.Internal}} {{.EnableIPv6}} {{json .IPAM.Config}}", rB.network); !strings.HasPrefix(got, "true false ") || strings.Contains(got, "Gateway") {
		t.Fatalf("attempt network gives the host an address or IPv6: %s", got)
	}
	if err := b.Write(ctx, "/workspace/AGENTS.md", []byte("other-workspace-private")); err != nil {
		t.Fatal(err)
	}
	// The project file lands in the agent working directory, readable by the agent.
	if err := b.Write(ctx, opencode.ProjectPath, []byte(`{"resources":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `cd /workspace/work && test "$(cat .multica/project/resources.json)" = '{"resources":[]}'`}); err != nil {
		t.Fatal("project file not in the agent working directory:", err)
	}
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `(while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nprivate" | nc -l -p 8080 -w 1; done) > /tmp/server.log 2>&1 < /dev/null &`}); err != nil {
		t.Fatal(err)
	}
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `wget -T 1 -q -O /tmp/local http://127.0.0.1:8080/prompt.txt; test "$(cat /tmp/local)" = private`}); err != nil {
		t.Fatal("other workspace listener was not serving", err)
	}
	ip := invoke("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", rB.name)
	if err := a.Execute(ctx, []string{"/bin/sh", "-c", "! wget -T 1 -q -O /tmp/other http://" + ip + ":8080/prompt.txt"}); err != nil {
		t.Fatal(err)
	}
	// Stream env reaches only that exec process: not container config, sibling execs or argv.
	const credential = "mat_relay_stream-env-sentinel"
	var streamed strings.Builder
	if err := a.Stream(ctx, []string{"/bin/sh", "-c", `printf %s "$MULTICA_TOKEN"`}, map[string]string{"MULTICA_TOKEN": credential}, func(_ io.Writer, r io.Reader) error {
		_, err := io.Copy(&streamed, r)
		return err
	}); err != nil || streamed.String() != credential {
		t.Fatalf("stream env not delivered: %v", err)
	}
	if strings.Contains(invoke("inspect", rA.name), credential) {
		t.Fatal("stream env persisted in container configuration")
	}
	if err := a.Execute(ctx, []string{"/bin/sh", "-c", `test -z "${MULTICA_TOKEN-}"`}); err != nil {
		t.Fatal("stream env leaked into a later exec")
	}
	// Stdin reaches the agent while its output is read; closing stdin ends it.
	var echoed string
	if err := a.Stream(ctx, []string{"cat"}, nil, func(w io.Writer, r io.Reader) error {
		if _, err := io.WriteString(w, "ping\n"); err != nil {
			return err
		}
		var err error
		echoed, err = bufio.NewReader(r).ReadString('\n')
		return err
	}); err != nil || echoed != "ping\n" {
		t.Fatalf("stream stdin not delivered: %q %v", echoed, err)
	}
	if err := a.Stream(ctx, []string{"true"}, map[string]string{"BAD=NAME": "x"}, func(io.Writer, io.Reader) error { return nil }); err == nil {
		t.Fatal("invalid environment name accepted")
	}
	// Cancelling the attempt leaves stdin open for the protocol's cancellation handshake.
	turn, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()
	if err := a.Stream(turn, []string{"/bin/sh", "-c", `echo ready; read message; echo "$message"`}, nil, func(w io.Writer, r io.Reader) error {
		reader := bufio.NewReader(r)
		if _, err := reader.ReadString('\n'); err != nil {
			return err
		}
		cancelTurn()
		// Let CommandContext observe cancellation before the protocol writes to stdin.
		time.Sleep(250 * time.Millisecond)
		if _, err := io.WriteString(w, "session/cancel\n"); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err == nil && line != "session/cancel\n" {
			return fmt.Errorf("cancellation was not acknowledged")
		}
		return err
	}); err != nil {
		t.Fatalf("transport ended before cancellation handshake: %v", err)
	}
	if err := a.Write(ctx, "/etc/forbidden", []byte("secret")); err == nil {
		t.Fatal("unapproved projection path")
	}
	// Skill trees land under the approved root as the attempt user, including non-ASCII paths.
	tree := map[string][]byte{"review/SKILL.md": []byte("---\nname: review\n---\n"), "review/references/обзор.md": []byte("ok"), "deploy/SKILL.md": nil}
	if err := a.WriteTree(ctx, "/workspace/config/opencode/skills", tree); err != nil {
		t.Fatal(err)
	}
	if err := a.Execute(ctx, []string{"/bin/sh", "-c", `set -e; cd /workspace/config/opencode/skills; test "$(cat review/references/обзор.md)" = ok; test -f deploy/SKILL.md
test "$(stat -c %u review/SKILL.md)" = 65532; test "$(find . | wc -l)" = 7`}); err != nil {
		t.Fatal("skill tree not projected:", err)
	}
	if a.WriteTree(ctx, "/workspace", tree) == nil || a.WriteTree(ctx, "/workspace/config/opencode/skills", map[string][]byte{"../x": nil}) == nil {
		t.Fatal("unapproved tree root or escaping path accepted")
	}
	if err := backend.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := invoke("ps", "-aq", "--filter", "label="+ownerLabel+"="+owner); got != "" {
		t.Fatal("owned execution survived startup")
	}
	if got := invoke("network", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner); got != "" {
		t.Fatal("owned projection network survived startup")
	}
}
