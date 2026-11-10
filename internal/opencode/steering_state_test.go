package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type nativeStateWorkload struct {
	stubWorkload
	root, bin            string
	captures, executions int
	before               func()
}

// Execute the production script unchanged; only its fixed workspace path
// arguments are remapped into this test's temporary directory.
func (w *nativeStateWorkload) command(ctx context.Context, args []string) ([]byte, error) {
	if len(args) < 3 || args[0] != "/bin/sh" || args[1] != "-c" {
		return nil, fmt.Errorf("unexpected native owner launch in state preparation")
	}
	if w.before != nil {
		w.before()
	}
	remapped := append([]string(nil), args...)
	for i := 3; i < len(remapped); i++ {
		remapped[i] = strings.ReplaceAll(remapped[i], filepath.Dir(SessionDB), w.root)
	}
	cmd := exec.CommandContext(ctx, remapped[0], remapped[1:]...)
	cmd.Dir = w.root
	cmd.Env = append(os.Environ(), "PATH="+w.bin+":/usr/bin:/bin")
	return cmd.CombinedOutput()
}

func (w *nativeStateWorkload) Capture(ctx context.Context, args []string, _ map[string]string) ([]byte, error) {
	w.captures++
	return w.command(ctx, args)
}

func (w *nativeStateWorkload) Execute(ctx context.Context, args []string) error {
	w.executions++
	_, err := w.command(ctx, args)
	return err
}

func nativeStateFixture(t *testing.T) (*running, *nativeStateWorkload) {
	t.Helper()
	dir := t.TempDir()
	w := &nativeStateWorkload{root: filepath.Join(dir, "attempt-state"), bin: filepath.Join(dir, "bin")}
	for _, path := range []string{w.root, w.bin} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Shell moves/removals are real. Controller file Sync is real too; avoid
	// flushing every filesystem on the developer's machine for each script.
	if err := os.WriteFile(filepath.Join(w.bin, "sync"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r := &running{adapter: &Adapter{Server: "https://multica.example.invalid", Steering: &multica.Client{}, OwnerState: filepath.Join(dir, "controller-state")}, task: safeTask(), workload: w, workDir: "retained-fixture-volume", resume: "ses_prior", nativePassword: "native-owner-password-sentinel"}
	if err := json.Unmarshal([]byte(`"mat_state_credential_sentinel"`), &r.task.AuthToken); err != nil {
		t.Fatal(err)
	}
	w.before = func() {
		owner := readNativeOwner(t, r)
		if owner.Clean || owner.Attempt != r.task.AttemptKey() {
			t.Fatal("workload action preceded the durable unclean owner record")
		}
	}
	return r, w
}

func readNativeOwner(t *testing.T, r *running) nativeOwner {
	t.Helper()
	data, err := os.ReadFile(r.nativeOwnerPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sentinel") || strings.Contains(string(data), "auth_token") || strings.Contains(string(data), "password") {
		t.Fatal("controller owner receipt persisted credentials")
	}
	var owner nativeOwner
	if json.Unmarshal(data, &owner) != nil {
		t.Fatal("invalid durable controller owner record")
	}
	info, err := os.Stat(r.nativeOwnerPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("controller owner record is not private")
	}
	return owner
}

func seedNativeDatabase(t *testing.T, w *nativeStateWorkload, suffixes []string) map[string]string {
	t.Helper()
	want := map[string]string{}
	for _, suffix := range suffixes {
		name, content := filepath.Base(SessionDB)+suffix, "old-native-state"+suffix
		if err := os.WriteFile(filepath.Join(w.root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		want[name] = content
	}
	return want
}

func assertQuarantinedNativeDatabase(t *testing.T, r *running, w *nativeStateWorkload, want map[string]string) {
	t.Helper()
	for name, content := range want {
		if _, err := os.Lstat(filepath.Join(w.root, name)); !os.IsNotExist(err) {
			t.Fatalf("old native file %s survived owner preparation", name)
		}
		matches, err := filepath.Glob(filepath.Join(w.root, "interrupted-*", name))
		if err != nil || len(matches) != 1 {
			t.Fatalf("native quarantine missing %s", name)
		}
		data, err := os.ReadFile(matches[0])
		if err != nil || string(data) != content {
			t.Fatalf("native quarantine changed %s", name)
		}
	}
	if r.resume != "" || r.retired != "ses_prior" {
		t.Fatal("quarantined native state retained its prior session")
	}
	if owner := readNativeOwner(t, r); owner.Clean {
		t.Fatal("new owner became clean before execution")
	}
}

func TestNativeStateUntrustedOwnerQuarantinesBeforeBoot(t *testing.T) {
	for _, record := range []string{"missing", "unclean", "malformed", "empty", "invalid-claim", "foreign-runtime", "foreign-workspace"} {
		for _, marker := range []string{"removed", "broken-link"} {
			t.Run(record+"/"+marker, func(t *testing.T) {
				r, w := nativeStateFixture(t)
				want := seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
				if record == "unclean" {
					if err := r.saveNativeOwner(false); err != nil {
						t.Fatal(err)
					}
				}
				if record == "malformed" || record == "empty" {
					if err := os.MkdirAll(r.adapter.OwnerState, 0700); err != nil {
						t.Fatal(err)
					}
					body := `{}`
					if record == "malformed" {
						body = `{"clean":true,`
					}
					if err := os.WriteFile(r.nativeOwnerPath(), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if record == "invalid-claim" || record == "foreign-runtime" || record == "foreign-workspace" {
					if err := os.MkdirAll(r.adapter.OwnerState, 0700); err != nil {
						t.Fatal(err)
					}
					previous := r.task
					switch record {
					case "foreign-runtime":
						previous.RuntimeID = "50000000-0000-4000-8000-000000000002"
					case "foreign-workspace":
						previous.WorkspaceID = "10000000-0000-4000-8000-000000000002"
					}
					attempt := previous.AttemptKey()
					if record == "invalid-claim" {
						attempt = ""
					}
					data, _ := json.Marshal(nativeOwner{Attempt: attempt, Clean: true})
					if err := os.WriteFile(r.nativeOwnerPath(), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if marker == "broken-link" {
					if err := os.Symlink(filepath.Join(w.root, "missing-marker-target"), filepath.Join(w.root, filepath.Base(httpAttemptMarker))); err != nil {
						t.Fatal(err)
					}
				}
				lost, err := r.prepareNativeState(context.Background())
				if err != nil || !lost {
					t.Fatalf("unknown/unclean native owner was trusted: %v", err)
				}
				assertQuarantinedNativeDatabase(t, r, w, want)
			})
		}
	}
}

func TestNativeStateOrphanedSidecarsRetireWithoutDatabaseMain(t *testing.T) {
	r, w := nativeStateFixture(t)
	want := seedNativeDatabase(t, w, []string{"-wal", "-shm"})
	if lost, err := r.prepareNativeState(context.Background()); err != nil || !lost {
		t.Fatalf("orphaned SQLite sidecars were not retired: %v", err)
	}
	assertQuarantinedNativeDatabase(t, r, w, want)
}

func TestNativeStateCleanResumePreservesDatabase(t *testing.T) {
	r, w := nativeStateFixture(t)
	want := seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
	if err := r.saveNativeOwner(true); err != nil {
		t.Fatal(err)
	}
	r.task.ID = "40000000-0000-4000-8000-000000000002"
	if lost, err := r.prepareNativeState(context.Background()); err != nil || lost {
		t.Fatalf("clean native owner lost normal resume: %v", err)
	}
	if r.resume != "ses_prior" || r.retired != "" {
		t.Fatal("clean retained session was retired")
	}
	for name, content := range want {
		data, err := os.ReadFile(filepath.Join(w.root, name))
		if err != nil || string(data) != content {
			t.Fatalf("clean resume changed %s", name)
		}
	}
	if err := r.cleanNativeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readNativeOwner(t, r).Clean || w.executions != 0 {
		t.Fatal("unproven owner was marked clean")
	}
	r.nativeClean = true
	if err := r.cleanNativeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !readNativeOwner(t, r).Clean || w.executions != 1 {
		t.Fatal("verified owner exit did not preserve clean resume")
	}
	if _, err := os.Lstat(filepath.Join(w.root, filepath.Base(httpAttemptMarker))); !os.IsNotExist(err) {
		t.Fatal("clean exit retained the diagnostic agent marker")
	}
}

func TestNativeStateControllerFailurePreventsWorkloadAction(t *testing.T) {
	r, w := nativeStateFixture(t)
	want := seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
	r.adapter.OwnerState = "relative-controller-state-not-allowed"
	if _, err := r.prepareNativeState(context.Background()); err == nil {
		t.Fatal("invalid controller state allowed an owner boot")
	}
	if w.captures != 0 || w.executions != 0 {
		t.Fatal("workload acted before controller ownership was durable")
	}
	for name, content := range want {
		data, err := os.ReadFile(filepath.Join(w.root, name))
		if err != nil || string(data) != content {
			t.Fatal("failed owner receipt mutated agent state")
		}
	}
}

func TestNativeStateCancelledPreparationRemainsUnclean(t *testing.T) {
	r, w := nativeStateFixture(t)
	want := seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.prepareNativeState(ctx); err == nil {
		t.Fatal("cancelled preparation succeeded")
	}
	if readNativeOwner(t, r).Clean {
		t.Fatal("cancelled preparation left trusted clean ownership")
	}
	if lost, err := r.prepareNativeState(context.Background()); err != nil || !lost {
		t.Fatalf("retry after cancelled preparation reused unknown state: %v", err)
	}
	assertQuarantinedNativeDatabase(t, r, w, want)
}

func TestNativeStateControllerWriteErrorPreventsOwnerBoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	r, w := nativeStateFixture(t)
	seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
	if err := os.MkdirAll(r.adapter.OwnerState, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.adapter.OwnerState, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(r.adapter.OwnerState, 0700)
	if _, err := r.prepareNativeState(context.Background()); err == nil {
		t.Fatal("unwritable controller state allowed an owner boot")
	}
	if w.captures != 0 || w.executions != 0 {
		t.Fatal("native owner preparation proceeded without durable ownership")
	}
	if _, err := os.Lstat(filepath.Join(w.root, filepath.Base(httpAttemptMarker))); !os.IsNotExist(err) {
		t.Fatal("failed controller write mutated the agent marker")
	}
}

func TestNativeStateCleanReceiptDoesNotCrossIdentityScope(t *testing.T) {
	for _, scope := range []string{"workspace", "agent", "volume", "server"} {
		t.Run(scope, func(t *testing.T) {
			r, w := nativeStateFixture(t)
			want := seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
			if err := r.saveNativeOwner(true); err != nil {
				t.Fatal(err)
			}
			prior := r.nativeOwnerPath()
			switch scope {
			case "workspace":
				r.task.WorkspaceID = "10000000-0000-4000-8000-000000000002"
			case "agent":
				agent := *r.task.Agent
				r.task.Agent = &agent
				r.task.AgentID, r.task.Agent.ID = "20000000-0000-4000-8000-000000000002", "20000000-0000-4000-8000-000000000002"
			case "volume":
				r.workDir = "another-retained-volume"
			case "server":
				r.adapter.Server = "https://another.example.invalid"
			}
			if r.nativeOwnerPath() == prior {
				t.Fatal("controller clean ownership crossed its identity scope")
			}
			if lost, err := r.prepareNativeState(context.Background()); err != nil || !lost {
				t.Fatalf("foreign clean owner authorized native resume: %v", err)
			}
			assertQuarantinedNativeDatabase(t, r, w, want)
		})
	}
}
