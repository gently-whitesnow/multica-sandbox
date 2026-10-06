//go:build containers

package docker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
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
	backend := &Projected{Backend: Backend{Image: testImage, Owner: owner, Command: []string{"/bin/sh"}}, Network: template, Peers: []string{peer}}
	t.Cleanup(func() {
		_ = backend.Reconcile(context.Background())
		_, _ = command(context.Background(), "rm", "-fv", peer)
		_, _ = command(context.Background(), "network", "rm", template)
	})
	a, err := backend.Start(ctx, "workspace-a:attempt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := backend.Start(ctx, "workspace-b:attempt")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Write(ctx, "/workspace/data/opencode/mcp-auth.json", []byte(`{"a":"private-a"}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Execute(ctx, []string{"/bin/sh", "-c", `test ! -e /workspace/data/opencode/mcp-auth.json; test ! -e /var/run/docker.sock; test ! -e /run/secrets; ! wget -T 1 -q -O /tmp/leak http://169.254.169.254/; ! wget -T 1 -q -O /tmp/leak http://1.1.1.1/`}); err != nil {
		t.Fatal(err)
	}
	rA, rB := a.(*projectedRun), b.(*projectedRun)
	if rA.network == rB.network {
		t.Fatal("shared attempt network")
	}
	if err := b.Write(ctx, "/workspace/prompt.txt", []byte("other-workspace-private")); err != nil {
		t.Fatal(err)
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
	if err := a.Stream(ctx, []string{"/bin/sh", "-c", `printf %s "$MULTICA_TOKEN"`}, map[string]string{"MULTICA_TOKEN": credential}, func(r io.Reader) error {
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
	if err := a.Stream(ctx, []string{"true"}, map[string]string{"BAD=NAME": "x"}, func(io.Reader) error { return nil }); err == nil {
		t.Fatal("invalid environment name accepted")
	}
	if err := a.Write(ctx, "/etc/forbidden", []byte("secret")); err == nil {
		t.Fatal("unapproved projection path")
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
