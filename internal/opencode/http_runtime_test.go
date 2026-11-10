package opencode

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Run explicitly with VERIFY_HTTP_RUNTIME=1 go test ./internal/opencode -run
// '^TestHTTPRuntime' -count=1 -timeout=90s. All owners and inputs are disposable.
func httpRuntimeContainer(t *testing.T, command string) string {
	t.Helper()
	if os.Getenv("VERIFY_HTTP_RUNTIME") != "1" {
		t.Skip("set VERIFY_HTTP_RUNTIME=1 for disposable native transport timing probes")
	}
	name := fmt.Sprintf("sandbox-http-runtime-%d", time.Now().UnixNano())
	image := Images[slices.Index(Supported, "2.2.5")]
	httpRuntimeDocker(t, "run", "-d", "--network=none", "--name", name, "--entrypoint", "/bin/sh", image, "-c", command)
	t.Cleanup(func() { httpRuntimeDocker(t, "rm", "-f", name) })
	return name
}

func httpRuntimeDocker(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("disposable docker command: %v", err)
	}
	return output
}

type runtimeHTTPWorkload struct {
	nativeHTTPWorkload
	container string
}

func (w *runtimeHTTPWorkload) StreamStderr(ctx context.Context, args []string, env map[string]string, stderr io.Writer, converse func(io.Writer, io.Reader) error) error {
	// Match the owner's detached process context. The callback must enforce the
	// finite request deadline rather than wait for the owner's exit grace.
	process, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	command := []string{"exec", "-i"}
	for name, value := range env {
		command = append(command, "--env", name+"="+value)
	}
	cmd := exec.CommandContext(process, "docker", append(append(command, w.container), args...)...)
	cmd.Stderr = stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	err := converse(stdin, stdout)
	_ = stdin.Close()
	if err != nil {
		cancel()
	}
	wait := cmd.Wait()
	if err != nil {
		return err
	}
	return wait
}

func TestHTTPRuntimeDeadlineAndEphemeralBodyCleanup(t *testing.T) {
	container := httpRuntimeContainer(t, "touch /tmp/stall-ready; tail -f /dev/null | nc -l -p 4096 > /tmp/held-request; sleep 180")
	httpRuntimeDocker(t, "exec", container, "/bin/sh", "-c", `while [ ! -e /tmp/stall-ready ]; do sleep 0.01; done`)
	r, _, _ := nativeHTTPFixture()
	r.workload = &runtimeHTTPWorkload{container: container}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := r.httpAPI(ctx, "POST", "/api/session/ses_fixture/prompt", map[string]string{"text": "generic pending guidance"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("request deadline: err=%v elapsed=%s stderr=%s", err, time.Since(started), r.stderr.String())
	}
	httpRuntimeDocker(t, "exec", container, "/bin/sh", "-c", `test -s /tmp/held-request; i=0; while [ "$i" -lt 40 ]; do set -- /tmp/native-http-*; [ ! -e "$1" ] && exit 0; i=$((i+1)); sleep 0.1; done; exit 1`)
	t.Logf("stalled request returned after %s; ephemeral body was removed", time.Since(started))
}

func TestHTTPRuntimeStdioEOFAndIdleSSE(t *testing.T) {
	container := httpRuntimeContainer(t, "mkdir -p /workspace/work; sleep 180")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	owner := exec.CommandContext(ctx, "docker", "exec", "-i", "--env", "OPENCODE_PASSWORD=fixture-password", container, "opencode", "serve", "--stdio", "--hostname", "127.0.0.1", "--port", "4096")
	stdin, _ := owner.StdinPipe()
	stdout, _ := owner.StdoutPipe()
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = owner.Process.Kill() }()
	remaining, err := nativeHTTPReady(ctx, stdout)
	if err != nil {
		t.Fatal(err)
	}
	go drainHTTPOutput(ctx, remaining)
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:fixture-password"))
	sse := exec.CommandContext(ctx, "docker", "exec", container, "wget", "-T", "0", "-q", "-O", "-", "--header", header, nativeHTTPOrigin+"/api/event")
	stream, _ := sse.StdoutPipe()
	if err := sse.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sse.Process.Kill(); _ = sse.Wait() }()
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	httpRuntimeHeartbeat(t, ctx, lines)
	httpRuntimePendingCancellation(t, ctx, container, lines)
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- owner.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stdio EOF owner exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio EOF did not stop the owner")
	}
	t.Log("native owner emitted idle heartbeat and exited0 after stdin EOF")
}

func httpRuntimePendingCancellation(t *testing.T, ctx context.Context, container string, lines <-chan string) {
	t.Helper()
	r, _, _ := nativeHTTPFixture()
	r.nativePassword = "fixture-password"
	r.workload = &runtimeHTTPWorkload{container: container}
	session, err := r.httpSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := "msg_runtimewithdrawfixture"
	body := map[string]any{"id": input, "text": "generic pending guidance", "delivery": "steer", "resume": false}
	if err := r.httpAPI(ctx, "POST", "/api/session/"+session+"/prompt", body, nil); err != nil {
		t.Fatal(err)
	}
	h := newHTTPTurn(r, session)
	done := make(chan error, 1)
	go func() { done <- h.withdraw(ctx, input, 0) }()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("native cancellation stream ended")
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var event httpEvent
				if json.Unmarshal([]byte(data), &event) == nil {
					h.consume(event)
				}
			}
		case err := <-done:
			if !errors.Is(err, errHTTPInputEnded) || h.unsafeResume || !h.cancelled[input] {
				t.Fatalf("native cancellation: err=%v unsafe=%v", err, h.unsafeResume)
			}
			h.terminal = true
			h.markClean(ctx)
			if !r.nativeClean {
				t.Fatalf("native pending cancellation prevented quiescent reuse: unsafe=%v busy=%v terminal=%v stderr=%s", h.unsafeResume, h.busy, h.terminal, r.stderr.String())
			}
			t.Log("native pending cancellation event proved withdrawal; same session remains reusable")
			return
		case <-ctx.Done():
			t.Fatal("native cancellation deadline")
		}
	}
}

func httpRuntimeHeartbeat(t *testing.T, ctx context.Context, lines <-chan string) {
	t.Helper()
	connected := false
	heartbeats := 0
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("idle SSE disconnected")
			}
			connected = connected || strings.Contains(line, "server.connected")
			if connected && line == ": heartbeat" {
				heartbeats++
				if heartbeats == 2 {
					return
				}
			}
		case <-ctx.Done():
			t.Fatal("idle SSE heartbeat deadline")
		}
	}
}

// A stopped owner leaves an admission unanswered; the request is cut at its
// deadline and the resumed owner still admits it. Resending under the same
// stable ID through the same code path must not create a second input.
func TestHTTPRuntimeLostAdmissionResponseIsDeduplicated(t *testing.T) {
	container := httpRuntimeContainer(t, "mkdir -p /workspace/work; sleep 180")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	owner := exec.CommandContext(ctx, "docker", "exec", "-i", "--env", "OPENCODE_PASSWORD=fixture-password", container, "opencode", "serve", "--stdio", "--hostname", "127.0.0.1", "--port", "4096")
	stdin, _ := owner.StdinPipe()
	stdout, _ := owner.StdoutPipe()
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = owner.Process.Kill() }()
	remaining, err := nativeHTTPReady(ctx, stdout)
	if err != nil {
		t.Fatal(err)
	}
	go drainHTTPOutput(ctx, remaining)
	r, _, _ := nativeHTTPFixture()
	r.nativePassword = "fixture-password"
	r.workload = &runtimeHTTPWorkload{container: container}
	session, err := r.httpSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shell := func(script string) string {
		return strings.TrimSpace(string(httpRuntimeDocker(t, "exec", container, "/bin/sh", "-c", script)))
	}
	signal := func(name string) {
		shell(`for d in /proc/[0-9]*; do tr '\0' ' ' < "$d/cmdline" 2>/dev/null | grep -q '^opencode serve' && kill -` + name + ` "${d#/proc/}"; done; true`)
	}
	inbox := func() []httpInboxItem {
		var response struct {
			Data []httpInboxItem `json:"data"`
		}
		if err := r.httpAPI(ctx, "GET", "/api/session/"+session+"/inbox", nil, &response); err != nil {
			t.Fatal(err)
		}
		return response.Data
	}
	input := "msg_runtimelostadmission"
	body := map[string]any{"id": input, "text": "generic pending guidance", "delivery": "steer", "resume": false}
	// A cold owner drops an admission whose client leaves during its first lazy
	// load. Attempts steer a session that already took its prompt, so warm it.
	warm := map[string]any{"id": "msg_runtimewarmup", "text": "generic earlier input", "delivery": "steer", "resume": false}
	if err = r.httpAPI(ctx, "POST", "/api/session/"+session+"/prompt", warm, nil); err != nil || len(inbox()) != 1 {
		t.Fatalf("warm-up admission: %v", err)
	}
	signal("STOP")
	failed := make(chan error, 1)
	go func() { failed <- r.httpAPI(ctx, "POST", "/api/session/"+session+"/prompt", body, nil) }()
	// The request sits unanswered in the stopped owner's socket; one second lets
	// it arrive, then the client is killed. (A client that times out by itself
	// is not admitted later; either way the resend below is safe.)
	cut := shell(`i=0; while [ "$i" -lt 100 ]; do pid=$(pgrep -x wget) && { sleep 1; kill -KILL $pid; echo cut; exit 0; }; i=$((i+1)); sleep 0.1; done; echo missing`)
	err = <-failed
	signal("CONT")
	if cut != "cut" || err == nil {
		t.Fatalf("admission was not cut while unanswered: %s %v", cut, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(inbox()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("resumed owner did not admit the request whose response was lost: stderr=%s ps=%s", r.stderr.String(), shell("ps -o pid,stat,args; ls -la /tmp"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err = r.httpAPI(ctx, "POST", "/api/session/"+session+"/prompt", body, nil); err != nil {
		t.Fatalf("resend under the stable ID: %v", err)
	}
	if items := inbox(); len(items) != 2 || items[0].ID == items[1].ID || items[0].ID != input && items[1].ID != input {
		t.Fatalf("stable ID did not deduplicate the resent admission: %d inputs", len(items))
	}
	t.Log("real OpenCode admitted an unanswered request once; the resend under the same ID added nothing")
}
