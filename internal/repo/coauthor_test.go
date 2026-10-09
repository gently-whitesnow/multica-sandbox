package repo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// localTarget runs attempt commands on the host with /workspace mapped to a directory.
type localTarget struct{ base string }

func (l localTarget) Capture(ctx context.Context, args []string, env map[string]string) ([]byte, error) {
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "/workspace", l.base)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

type fakeSettings struct {
	mu      sync.Mutex
	enabled bool
	err     error
}

func (f *fakeSettings) CoAuthoredBy(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled, f.err
}

func (f *fakeSettings) set(enabled bool, err error) {
	f.mu.Lock()
	f.enabled, f.err = enabled, err
	f.mu.Unlock()
}

// A live attempt follows setting changes: checkouts the sweep reaches get their hook
// reconciled, deeper ones through the state file, and foreign hooks stay untouched.
func TestSettingChangeReachesLiveHooks(t *testing.T) {
	const ws, task = "10000000-0000-4000-8000-000000000001", "a3000000-0000-4000-8000-000000000042"
	bare := origin(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"work/a/b", "work/user"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	git(t, filepath.Join(base, "work/user"), "init", "-q")
	foreign := filepath.Join(base, "work/user/.git/hooks/prepare-commit-msg")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	settings := &fakeSettings{enabled: true}
	c := &Checkout{Auth: fakeAuth("opaque"), Settings: settings}
	c.Register("attempt", Task{Workspace: ws, ID: task, AgentName: "A", Repos: map[string]string{bare: ""}, Target: localTarget{base}})
	c.Refresh(context.Background(), "attempt")
	var paths []string
	for _, workdir := range []string{WorkDir, WorkDir + "/a/b"} {
		r := httptest.NewRequest(http.MethodPost, "/repo/checkout", strings.NewReader(`{"url":"`+bare+`","workspace_id":"`+ws+`","task_id":"`+task+`","workdir":"`+workdir+`"}`))
		r.Header.Set("Authorization", "Bearer opaque")
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("checkout %s: %d %s", workdir, w.Code, w.Body)
		}
		paths = append(paths, filepath.Join(base, strings.TrimPrefix(workdir, "/workspace"), "fixture"))
	}
	trailers := func(want bool, step string) {
		t.Helper()
		for _, path := range paths {
			run(t, path, "commit", "-q", "--allow-empty", "-m", step)
			if got := strings.TrimSpace(run(t, path, "log", "-1", "--format=%(trailers:key=Co-authored-by,valueonly)")); got != map[bool]string{true: "multica-agent <github@multica.ai>"}[want] {
				t.Fatalf("%s: %s trailer %q", step, path, got)
			}
		}
		if data, err := os.ReadFile(foreign); err != nil || string(data) != "#!/bin/sh\n" {
			t.Fatalf("%s: foreign hook changed", step)
		}
	}
	trailers(true, "enabled at checkout")
	settings.set(false, nil)
	c.Refresh(context.Background(), "attempt")
	if _, err := os.Stat(filepath.Join(paths[0], ".git/hooks/prepare-commit-msg")); !os.IsNotExist(err) {
		t.Fatal("disabled setting left the swept hook")
	}
	trailers(false, "disabled during the attempt")
	settings.set(true, errors.New("outage"))
	c.Refresh(context.Background(), "attempt")
	trailers(false, "failed read keeps the setting")
	settings.set(true, nil)
	c.Refresh(context.Background(), "attempt")
	trailers(true, "enabled again")
}
