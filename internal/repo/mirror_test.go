package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorKeyNormalizesRemoteForms(t *testing.T) {
	key, ok := MirrorKey("https://Git.Example.test/team/fixture.git")
	if !ok || !mirrorName(key) {
		t.Fatalf("key %q %v", key, ok)
	}
	for _, same := range []string{"https://git.example.test/team/fixture", "https://git.example.test/team/fixture/", "git@git.example.test:team/fixture.git", "ssh://deploy@git.example.test:2222/team/fixture.git"} {
		if got, _ := MirrorKey(same); got != key {
			t.Errorf("%s: %s, want %s", same, got, key)
		}
	}
	for _, other := range []string{"https://git.example.test/team/other.git", "https://other.example.test/team/fixture.git", "https://git.example.test/Team/fixture.git"} {
		if got, _ := MirrorKey(other); got == key {
			t.Errorf("%s shares the mirror", other)
		}
	}
	for _, invalid := range []string{"", "ext::sh -c touch% /tmp/x", "file:///srv/fixture.git", "https://user:secret@git.example.test/team/fixture.git", "https://git.example.test/team/../fixture.git", "https://git.example.test/"} {
		if _, ok := MirrorKey(invalid); ok {
			t.Errorf("%q accepted", invalid)
		}
	}
}

func mirrorName(key string) bool {
	return len(key) == 32 && strings.Trim(key, "0123456789abcdef") == ""
}

// refreshMirror runs MirrorScript as the refresh container does, against a local origin.
func refreshMirror(t *testing.T, dir, url string) {
	t.Helper()
	if out, err := runMirror(dir, url); err != nil {
		t.Fatalf("mirror: %v\n%s", err, out)
	}
}

// runMirror passes a credential the way relay grants do: in the environment only.
func runMirror(dir, url string) ([]byte, error) {
	cmd := exec.Command("/bin/sh", "-c", MirrorScript, "mirror", dir, url)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Bearer grant-secret")
	return cmd.CombinedOutput()
}

// corrupt overwrites every object file of a repository.
func corrupt(t *testing.T, repo string) {
	t.Helper()
	if err := filepath.WalkDir(filepath.Join(repo, "objects"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(path, "info") {
			return err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("corrupt"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorScriptCreatesAndFetches(t *testing.T) {
	bare := origin(t)
	mirror := filepath.Join(t.TempDir(), "0123")
	if err := os.MkdirAll(mirror+".new/objects", 0700); err != nil {
		t.Fatal(err)
	}
	refreshMirror(t, mirror, bare)
	if _, err := os.Stat(mirror + ".new"); !os.IsNotExist(err) || git(t, mirror, "rev-parse", "main") != git(t, bare, "rev-parse", "main") ||
		git(t, mirror, "rev-parse", "v1") != git(t, bare, "rev-parse", "v1") || git(t, mirror, "config", "core.bare") != "true" {
		t.Fatal("first fetch did not create a bare mirror of branches and tags")
	}
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "-q", bare, work)
	git(t, work, "commit", "-q", "--allow-empty", "-m", "third")
	git(t, work, "push", "-q", "origin", "main", ":feature")
	refreshMirror(t, mirror, bare)
	if git(t, mirror, "rev-parse", "main") != git(t, bare, "rev-parse", "main") || git(t, mirror, "for-each-ref", "refs/heads/feature") != "" {
		t.Fatal("refresh did not follow the upstream branches")
	}
	// A refresh cut while holding a ref lock does not block the next one.
	git(t, work, "commit", "-q", "--allow-empty", "-m", "fourth")
	git(t, work, "push", "-q", "origin", "main")
	if err := os.WriteFile(filepath.Join(mirror, "refs", "heads", "main.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	refreshMirror(t, mirror, bare)
	if git(t, mirror, "rev-parse", "main") != git(t, bare, "rev-parse", "main") {
		t.Fatal("stale lock blocked the refresh")
	}
	// An unreachable origin keeps the mirror; an unreadable mirror is rebuilt.
	if out, err := runMirror(mirror, filepath.Join(t.TempDir(), "absent")); err == nil || strings.Contains(string(out), "rebuilding") {
		t.Fatalf("unreachable origin: %v %s", err, out)
	}
	if git(t, mirror, "rev-parse", "main") != git(t, bare, "rev-parse", "main") {
		t.Fatal("failed refresh lost the mirror")
	}
	corrupt(t, mirror)
	if out, err := runMirror(mirror, bare); err != nil || !strings.Contains(string(out), "rebuilding") {
		t.Fatalf("corrupt mirror: %v %s", err, out)
	}
	git(t, mirror, "fsck", "--full", "--no-dangling")
	for _, suffix := range []string{".new", ".old"} {
		if _, err := os.Stat(mirror + suffix); !os.IsNotExist(err) {
			t.Fatalf("rebuild left %s", suffix)
		}
	}
	// The grant and the URL stay out of the mirror, as the checkout's remote does.
	if err := filepath.WalkDir(mirror, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(path, "objects") {
			return err
		}
		data, err := os.ReadFile(path)
		if bytes.Contains(data, []byte("grant-secret")) || bytes.Contains(data, []byte(bare)) {
			t.Errorf("%s holds the credential or URL", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(mirror, "hooks")); len(entries) > 0 {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".sample") {
				t.Fatalf("mirror has hook %s", e.Name())
			}
		}
	}
}

// A new checkout borrows the mirror's objects and dissociates; a missing, broken or unrelated
// mirror leaves the plain clone from origin.
func TestCheckoutWithMirror(t *testing.T) {
	bare := origin(t)
	mirror := filepath.Join(t.TempDir(), "mirror")
	refreshMirror(t, mirror, bare)
	unrelated := filepath.Join(t.TempDir(), "unrelated")
	refreshMirror(t, unrelated, origin(t))
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "objects"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// Git reads unreadable mirror objects as missing and fetches them from origin.
	damaged := filepath.Join(t.TempDir(), "corrupt")
	refreshMirror(t, damaged, bare)
	corrupt(t, damaged)
	// A logging git on PATH shows which clone produced the checkout; with MIRROR_FAIL a
	// reference clone fails after writing part of the checkout.
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "git.log")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n[ \"$1\" != clone ] || echo \"$*\" >>"+log+"\n"+
		"case \"$MIRROR_FAIL $*\" in 1*--reference-if-able*) for last; do :; done; mkdir -p \"$last/.git\"; exit 1 ;; esac\nexec "+real+" \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const branch = "agent/fixture/000000000001"
	for name, c := range map[string]struct{ reference, clones, fail string }{
		"mirror":    {mirror, "reference", ""},
		"missing":   {filepath.Join(broken, "absent"), "reference", ""},
		"broken":    {broken, "reference", ""},
		"corrupt":   {damaged, "reference", ""},
		"unrelated": {unrelated, "reference", ""},
		"failing":   {mirror, "reference plain", "1"},
		"none":      {"", "plain", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MIRROR_FAIL", c.fail)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(log)
			got, kind, message := checkoutWith(t, c.reference, root, bare, "", branch, false, root, true)
			if kind != "" || got.Path != filepath.Join(root, "fixture") {
				t.Fatalf("checkout: %+v %s %s", got, kind, message)
			}
			data, _ := os.ReadFile(log)
			clones := []string{}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.Contains(line, "--reference-if-able "+c.reference+" --dissociate") {
					clones = append(clones, "reference")
				} else if line != "" {
					clones = append(clones, "plain")
				}
			}
			if strings.Join(clones, " ") != c.clones {
				t.Fatalf("clones %v, want %s", clones, c.clones)
			}
			if git(t, got.Path, "rev-parse", "HEAD") != git(t, bare, "rev-parse", "main") || git(t, got.Path, "remote", "get-url", "origin") != bare {
				t.Fatal("checkout is not the origin's default branch")
			}
			if _, err := os.Stat(filepath.Join(got.Path, ".git/objects/info/alternates")); !os.IsNotExist(err) {
				t.Fatal("checkout still borrows from the mirror")
			}
			git(t, got.Path, "fsck", "--full", "--no-dangling")
		})
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := checkoutWith(t, mirror, root, bare, "feature", branch, false, root, true)
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatal(err)
	}
	git(t, got.Path, "fsck", "--full", "--no-dangling")
	if git(t, got.Path, "rev-parse", "HEAD") != git(t, bare, "rev-parse", "feature") {
		t.Fatal("ref not resolved from origin")
	}
}

type fakeMirror struct {
	asked    []string
	ok       bool
	released int
}

func (m *fakeMirror) Ensure(_ context.Context, workspace, url string) (string, func(), bool) {
	m.asked = append(m.asked, workspace+" "+url)
	if !m.ok {
		return "", nil, false
	}
	return "0123456789abcdef0123456789abcdef", func() { m.released++ }, true
}

func TestCheckoutPassesReferenceMirror(t *testing.T) {
	const ws, task, url = "10000000-0000-4000-8000-000000000001", "a3000000-0000-4000-8000-000000000042", "https://git.example.test/team/fixture.git"
	target := &fakeTarget{out: "path /workspace/work/fixture\nbranch agent/a/000000000042\n"}
	mirrors := &fakeMirror{ok: true}
	c := &Checkout{Auth: fakeAuth("opaque"), Mirrors: mirrors}
	c.Register("attempt", Task{Workspace: ws, ID: task, AgentName: "A", Repos: map[string]string{url: ""}, Target: target})
	call := func() int {
		data, _ := json.Marshal(map[string]any{"url": url, "workspace_id": ws, "task_id": task, "workdir": WorkDir})
		r := httptest.NewRequest(http.MethodPost, "/repo/checkout", strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer opaque")
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		return w.Code
	}
	if call() != 200 || target.args[15] != MirrorDir+"/0123456789abcdef0123456789abcdef" || strings.Join(mirrors.asked, ",") != ws+" "+url || mirrors.released != 1 {
		t.Fatalf("mirror argument %q, asked %v", target.args[15], mirrors.asked)
	}
	mirrors.ok = false
	if call() != 200 || target.args[15] != "" {
		t.Fatalf("unavailable mirror passed: %q", target.args[15])
	}
}
