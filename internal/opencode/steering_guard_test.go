package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func TestNativeStateDisableSteeringRetiresHTTPBeforeACP(t *testing.T) {
	for _, evidence := range []string{"clean-receipt", "unclean-receipt", "marker-only"} {
		t.Run(evidence, func(t *testing.T) {
			r, w := nativeStateFixture(t)
			seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
			if evidence == "marker-only" {
				if err := os.WriteFile(filepath.Join(w.root, filepath.Base(httpAttemptMarker)), []byte("active"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := r.saveNativeOwner(evidence == "clean-receipt"); err != nil {
				t.Fatal(err)
			}
			w.before = nil
			r.adapter.Steering = nil
			lost, err := r.prepareNativeState(context.Background())
			if err != nil || !lost || r.resume != "" || r.retired != "ses_prior" {
				t.Fatalf("ACP reused HTTP owner: lost=%v err=%v", lost, err)
			}
			if _, err := os.Stat(filepath.Join(w.root, "opencode.db")); !os.IsNotExist(err) {
				t.Fatal("HTTP database survived transport change")
			}
			if _, err := os.Stat(r.nativeOwnerPath()); !os.IsNotExist(err) {
				t.Fatal("HTTP controller receipt survived ACP handover")
			}
			// Subsequent ordinary ACP follow-ups retain their own newly created state.
			seedNativeDatabase(t, w, []string{""})
			r.task.ID = "40000000-0000-4000-8000-000000000002"
			r.resume, r.retired = "ses_acp", ""
			if lost, err := r.prepareNativeState(context.Background()); err != nil || lost || r.resume != "ses_acp" {
				t.Fatalf("normal ACP continuity changed: %v", err)
			}
			// Returning to HTTP cannot mistake the old clean receipt for ACP quiescence.
			r.adapter.Steering = &multica.Client{}
			if lost, err := r.prepareNativeState(context.Background()); err != nil || !lost {
				t.Fatalf("HTTP trusted ACP-owned state: %v", err)
			}
		})
	}
}

func TestNativeStateDefaultACPWithoutHTTPHistoryKeepsDatabase(t *testing.T) {
	r, w := nativeStateFixture(t)
	r.adapter.Steering = nil
	w.before = nil
	want := seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
	if lost, err := r.prepareNativeState(context.Background()); err != nil || lost || r.resume != "ses_prior" {
		t.Fatalf("default ACP lost continuity: %v", err)
	}
	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(w.root, name))
		if err != nil || string(got) != content {
			t.Fatalf("ACP database changed: %s", name)
		}
	}
	if _, err := os.Stat(r.nativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatal("ordinary ACP unexpectedly acquired an HTTP owner receipt")
	}
}

func TestNativeStateQuarantineRetentionIsBoundedAndDoesNotFollowLinks(t *testing.T) {
	r, w := nativeStateFixture(t)
	outside := filepath.Join(t.TempDir(), "preserve")
	if err := os.WriteFile(outside, []byte("user-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(w.root, "interrupted-0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(w.root, "interrupted-user-notes")
	if err := os.Mkdir(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"40000000-0000-4000-8000-000000000001", "40000000-0000-4000-8000-000000000002", "40000000-0000-4000-8000-000000000003"} {
		r.task.ID = id
		seedNativeDatabase(t, w, []string{"", "-wal", "-shm"})
		if lost, err := r.prepareNativeState(context.Background()); err != nil || !lost {
			t.Fatalf("unclean owner not isolated: %v", err)
		}
		matches, err := filepath.Glob(filepath.Join(w.root, "interrupted-*", "opencode.db"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("quarantines accumulated: %v %v", matches, err)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "user-data" {
		t.Fatal("quarantine pruning followed an agent-controlled symlink")
	}
	if _, err := os.Stat(userDir); err != nil {
		t.Fatal("pruning removed an unrelated workdir directory")
	}
}
