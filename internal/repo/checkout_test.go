package repo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "init.defaultBranch=main"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin creates a bare repository with main, a feature branch and a tag.
func origin(t *testing.T) string {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	bare := filepath.Join(dir, "fixture.git")
	git(t, dir, "init", "-q", work)
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "README")
	git(t, work, "commit", "-q", "-m", "init")
	git(t, work, "tag", "v1")
	git(t, work, "checkout", "-q", "-b", "feature")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "feature")
	git(t, work, "checkout", "-q", "main")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "second")
	git(t, dir, "clone", "-q", "--bare", work, bare)
	return bare
}

func checkout(t *testing.T, root, url, ref, branch string, fresh bool, workdir string) (response, string, string) {
	t.Helper()
	flag := "0"
	if fresh {
		flag = "1"
	}
	cmd := exec.Command("/bin/sh", "-c", script, "checkout", url, ref, branch, flag, workdir, Name(url), root)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, _ := cmd.Output()
	return parse(out)
}

func TestCheckoutScriptFollowsUpstream(t *testing.T) {
	bare := origin(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const branch = "agent/fixture/000000000001"
	got, kind, message := checkout(t, root, bare, "", branch, false, root)
	path := filepath.Join(root, "fixture")
	if kind != "" || got != (response{Path: path, BranchName: branch}) {
		t.Fatalf("new checkout: %+v %s %s", got, kind, message)
	}
	if git(t, path, "rev-parse", "HEAD") != git(t, bare, "rev-parse", "main") || git(t, path, "config", "multica.checkout-mode") != "isolated" ||
		git(t, path, "for-each-ref", "--format=%(refname)", "refs/heads/") != "refs/heads/"+branch || git(t, path, "remote", "get-url", "origin") != bare {
		t.Fatal("new checkout is not an isolated branch from the default branch")
	}
	exclude, _ := os.ReadFile(filepath.Join(path, ".git/info/exclude"))
	if !strings.Contains(string(exclude), "\nAGENTS.md\n") || !strings.Contains(string(exclude), "\n.opencode\n") {
		t.Fatal("agent files are not excluded")
	}
	if got, _, _ := checkout(t, root, bare, "", branch, false, root); got.Kept != "task_branch" || got.BranchName != branch {
		t.Fatalf("task branch not kept: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(path, "draft"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, path, "commit", "-q", "--allow-empty", "-m", "local")
	git(t, path, "checkout", "-q", "-b", "elsewhere")
	if got, _, _ := checkout(t, root, bare, "", branch, false, root); got != (response{Path: path, BranchName: "elsewhere", Kept: "local_work", UncommittedFiles: 1, UnpushedCommits: 1}) {
		t.Fatalf("local work not kept: %+v", got)
	}
	// Fresh discards untracked files and starts a suffixed branch; commits stay on the old one.
	got, kind, _ = checkout(t, root, bare, "v1", branch, true, root)
	if kind != "" || !strings.HasPrefix(got.BranchName, branch+"-") || got.Kept != "" {
		t.Fatalf("fresh checkout: %+v %s", got, kind)
	}
	if _, err := os.Stat(filepath.Join(path, "draft")); !os.IsNotExist(err) || git(t, path, "rev-parse", "HEAD") != git(t, bare, "rev-parse", "v1^{commit}") {
		t.Fatal("fresh checkout did not reset to the requested tag")
	}
	if !strings.Contains(git(t, path, "for-each-ref", "--format=%(refname)", "refs/heads/"), "refs/heads/elsewhere") {
		t.Fatal("branch with unpushed commits was pruned")
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if got, kind, _ := checkout(t, root, bare, "feature", branch, false, nested); kind != "" || git(t, got.Path, "rev-parse", "HEAD") != git(t, bare, "rev-parse", "feature") {
		t.Fatal("branch ref not resolved through origin")
	}
	if err := os.Mkdir(filepath.Join(nested, "fresh"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ ref, workdir, kind string }{
		"symlink escape": {"", filepath.Join(root, "escape"), "forbidden"},
		"unknown ref":    {"missing", filepath.Join(root, "nested", "fresh"), "failed"},
	} {
		if _, kind, _ := checkout(t, root, bare, c.ref, branch, false, c.workdir); kind != c.kind {
			t.Fatalf("%s: got %q", name, kind)
		}
	}
	if err := os.Mkdir(filepath.Join(nested, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, kind, _ := checkout(t, root, filepath.Join(filepath.Dir(bare), "other.git"), "", branch, false, nested); kind != "conflict" {
		t.Fatal("foreign directory was reused as a checkout")
	}
	if _, kind, _ := checkout(t, root, filepath.Join(filepath.Dir(bare), "absent.git"), "", branch, false, root); kind != "failed" {
		t.Fatal("failed clone reported success")
	} else if _, err := os.Stat(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Fatal("failed clone left a partial checkout")
	}
}

func TestNaming(t *testing.T) {
	for agent, want := range map[string]string{"Relay Fixture Agent 7": "relay-fixture-agent-7", "  ": "agent", "Ünïcode!!": "n-code", strings.Repeat("ab-", 20): "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab"} {
		if got := Branch(agent, "a3000000-0000-4000-8000-000000000042"); got != "agent/"+want+"/000000000042" {
			t.Errorf("%q: %s", agent, got)
		}
	}
	for url, want := range map[string]string{"https://h/o/repo.git": "repo", "https://h/o/repo/": "repo", "git@h:o/r.git": "r", "https://h/": "h"} {
		if got := Name(url); got != want {
			t.Errorf("%q: %s", url, got)
		}
	}
}

type fakeTarget struct {
	args []string
	env  map[string]string
	out  string
	wait chan struct{}
}

func (f *fakeTarget) Capture(ctx context.Context, args []string, env map[string]string) ([]byte, error) {
	f.args, f.env = args, env
	if f.wait != nil {
		<-f.wait
	}
	return []byte(f.out), nil
}

type fakeAuth string

func (a fakeAuth) Authorize(r *http.Request) (relay.Lease, bool) {
	if r.Header.Get("Authorization") != "Bearer "+string(a) {
		return relay.Lease{}, false
	}
	return relay.Lease{Attempt: "attempt", Context: context.Background(), Release: func() {}}, true
}

func TestCheckoutHandler(t *testing.T) {
	const ws, task, url = "10000000-0000-4000-8000-000000000001", "a3000000-0000-4000-8000-000000000042", "https://git.example.test/team/fixture.git"
	target := &fakeTarget{out: "path /workspace/work/fixture\nbranch agent/a/000000000042\n"}
	c := &Checkout{Auth: fakeAuth("opaque")}
	c.Register("attempt", Task{Workspace: ws, ID: task, AgentName: "A", Repos: map[string]string{url: "release"}, Target: target, Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"}})
	call := func(token string, body map[string]any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/repo/checkout", strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		return w
	}
	valid := func() map[string]any {
		return map[string]any{"url": " " + url + " ", "workspace_id": ws, "task_id": task, "workdir": WorkDir, "agent_name": "ignored", "retry_busy": true}
	}
	w := call("opaque", valid())
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"path":"/workspace/work/fixture","branch_name":"agent/a/000000000042"}` {
		t.Fatalf("checkout: %d %s", w.Code, w.Body)
	}
	if got := target.args[4:]; strings.Join(got, "|") != url+"|release|agent/a/000000000042|0|"+WorkDir+"|fixture|"+WorkDir || target.env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("script arguments: %q", got)
	}
	for name, c := range map[string]struct {
		token  string
		change map[string]any
		code   int
	}{
		"credential":   {"other", nil, 401},
		"workspace":    {"opaque", map[string]any{"workspace_id": "10000000-0000-4000-8000-000000000002"}, 403},
		"task":         {"opaque", map[string]any{"task_id": ""}, 403},
		"mode":         {"opaque", map[string]any{"checkout_mode": "linked"}, 400},
		"outside":      {"opaque", map[string]any{"workdir": "/workspace"}, 403},
		"traversal":    {"opaque", map[string]any{"workdir": WorkDir + "/../data"}, 403},
		"repository":   {"opaque", map[string]any{"url": "https://git.example.test/team/other.git"}, 400},
		"equivalent":   {"opaque", map[string]any{"url": "https://git.example.test/team/fixture"}, 400},
		"option ref":   {"opaque", map[string]any{"ref": "--upload-pack=touch /tmp/x"}, 400},
		"range ref":    {"opaque", map[string]any{"ref": "main..HEAD"}, 400},
		"missing url":  {"opaque", map[string]any{"url": " "}, 400},
		"missing work": {"opaque", map[string]any{"workdir": ""}, 400},
	} {
		body := valid()
		for k, v := range c.change {
			body[k] = v
		}
		if got := call(c.token, body).Code; got != c.code {
			t.Errorf("%s: %d, want %d", name, got, c.code)
		}
	}
	target.out = "error forbidden /etc is outside the active task workdir /workspace/work\n"
	if w := call("opaque", valid()); w.Code != 403 || !strings.Contains(w.Body.String(), "not owned by the active task") {
		t.Fatalf("script refusal: %d %s", w.Code, w.Body)
	}
	busyWait = 200 * time.Millisecond
	target.out, target.wait = "path x\n", make(chan struct{})
	done := make(chan int)
	go func() { done <- call("opaque", valid()).Code }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if w := call("opaque", valid()); w.Code != 503 || w.Header().Get("X-Multica-Retryable") != "repo-busy" || time.Since(start) < busyWait {
		t.Fatalf("busy checkout: %d %v", w.Code, w.Header())
	}
	close(target.wait)
	if <-done != 200 {
		t.Fatal("first checkout failed")
	}
	c.Unregister("attempt")
	if call("opaque", valid()).Code != 401 {
		t.Fatal("unregistered attempt served")
	}
}
