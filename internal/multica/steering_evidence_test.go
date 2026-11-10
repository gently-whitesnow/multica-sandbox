package multica

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func evidenceFile(t *testing.T, dir, name string, size int64, age time.Duration) string {
	t.Helper()
	if filepath.Ext(name) == ".json" {
		name = fileName(name)
	}
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	stamp := time.Now().Add(-age)
	if err = os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func evidenceDir(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "supplements")
	quarantine := filepath.Join(dir, supplementEvidenceDir)
	if err := os.MkdirAll(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	return dir, quarantine
}

func coldStart(t *testing.T, dir string) *Client {
	t.Helper()
	c, err := New("https://multica.example.invalid", "controller_fixture_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.UseSupplements(dir); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSupplementEvidenceColdStartCollectsOnlyExpiredEvidence(t *testing.T) {
	dir, quarantine := evidenceDir(t)
	outside := evidenceFile(t, t.TempDir(), "outside.json", 8, 90*24*time.Hour)
	expired := evidenceFile(t, quarantine, "expired.json", 8, supplementEvidenceAge+time.Hour)
	corrupted := evidenceFile(t, quarantine, "corrupted.json", 3, supplementEvidenceAge+time.Hour)
	if err := os.WriteFile(corrupted, []byte("\x00{]"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-supplementEvidenceAge - time.Hour)
	_ = os.Chtimes(corrupted, old, old)
	fresh := evidenceFile(t, quarantine, "fresh.json", 8, time.Hour)
	foreign := evidenceFile(t, quarantine, "notes.txt", 8, 90*24*time.Hour)
	foreignJSON := filepath.Join(quarantine, "notes.json")
	if err := os.WriteFile(foreignJSON, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(foreignJSON, old, old)
	staleTemp := evidenceFile(t, quarantine, supplementTemp+"stale", 8, 2*supplementTempAge)
	liveTemp := evidenceFile(t, quarantine, supplementTemp+"live", 8, time.Second)
	link := filepath.Join(quarantine, "link.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(quarantine, "nested.json")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	// Active and replayable journals live one level up and are never candidates.
	active := evidenceFile(t, dir, fileName("active"), 8, 90*24*time.Hour)
	journalTemp := evidenceFile(t, dir, supplementTemp+"stale", 8, 2*supplementTempAge)
	coldStart(t, dir)
	for _, removed := range []string{expired, corrupted, staleTemp, journalTemp} {
		if exists(removed) {
			t.Errorf("cold start kept %s", filepath.Base(removed))
		}
	}
	for _, kept := range []string{fresh, foreign, foreignJSON, liveTemp, link, nested, outside, active} {
		if !exists(kept) {
			t.Errorf("cold start removed %s", filepath.Base(kept))
		}
	}
}

func TestSupplementEvidenceEntryAndByteBudgetsEvictOldestFirst(t *testing.T) {
	dir, quarantine := evidenceDir(t)
	for n := range supplementEvidenceEntries + 8 {
		evidenceFile(t, quarantine, fmt.Sprintf("%04d.json", n), 16, time.Duration(n)*time.Minute)
	}
	c := coldStart(t, dir)
	if names := supplementEvidenceNames(t, dir); len(names) != supplementEvidenceEntries || !exists(filepath.Join(quarantine, fileName("0000.json"))) || exists(filepath.Join(quarantine, fileName(fmt.Sprintf("%04d.json", supplementEvidenceEntries)))) {
		t.Fatalf("entry budget kept %d entries or evicted the newest", len(names))
	}
	if err := os.RemoveAll(quarantine); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(quarantine, 0700)
	for n := range 70 {
		evidenceFile(t, quarantine, fmt.Sprintf("%04d.json", n), 1<<20, time.Duration(n)*time.Minute)
	}
	if removed, freed, err := c.supplements.collect(time.Now()); err != nil || removed != 6 || freed != 6<<20 {
		t.Fatalf("byte budget: removed=%d freed=%d err=%v", removed, freed, err)
	}
	if !exists(filepath.Join(quarantine, fileName("0063.json"))) || exists(filepath.Join(quarantine, fileName("0064.json"))) {
		t.Fatal("byte budget did not evict strictly oldest first")
	}
}

func TestSupplementEvidencePeriodicCollection(t *testing.T) {
	dir, quarantine := evidenceDir(t)
	c := coldStart(t, dir)
	now := time.Now()
	c.supplements.now = func() time.Time { return now }
	c.supplements.collected = now
	expired := evidenceFile(t, quarantine, "expired.json", 8, supplementEvidenceAge+time.Hour)
	if _, err := c.ReplaySupplementAcks(context.Background()); err != nil || !exists(expired) {
		t.Fatal("collection ran before its interval", err)
	}
	now = now.Add(supplementEvidenceInterval)
	// A cancelled context does not skip or interrupt the synchronous pass.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ReplaySupplementAcks(ctx); err != nil || exists(expired) {
		t.Fatal("periodic replay did not collect expired evidence", err)
	}
}

func TestSupplementEvidenceRefusesSymlinkedQuarantine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "supplements")
	target := t.TempDir()
	victim := evidenceFile(t, target, "victim.json", 8, supplementEvidenceAge+time.Hour)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, supplementEvidenceDir)); err != nil {
		t.Fatal(err)
	}
	c := coldStart(t, dir)
	if _, _, err := c.supplements.collect(time.Now()); err == nil || !exists(victim) {
		t.Fatal("collection followed a symlinked quarantine", err)
	}
	record := supplementRecord{Version: 1, Task: testID, Current: &taskSupplement{CommentID: testID}}
	if err := c.supplements.quarantine(record, "receipt_none"); err == nil {
		t.Fatal("evidence was written through a symlinked quarantine")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 1 {
		t.Fatal("symlink target changed")
	}
}

// An interrupted pass or write leaves a consistent directory: the next cold
// start finishes the collection and drops the orphaned temporary file, while
// concurrent quarantine writes are never collected.
func TestSupplementEvidenceInterruptionAndConcurrentWrites(t *testing.T) {
	dir, quarantine := evidenceDir(t)
	for n := range 6 {
		evidenceFile(t, quarantine, fmt.Sprintf("expired-%d.json", n), 8, supplementEvidenceAge+time.Hour)
	}
	// A pass killed after two removals and a write killed before its rename.
	_ = os.Remove(filepath.Join(quarantine, fileName("expired-0.json")))
	_ = os.Remove(filepath.Join(quarantine, fileName("expired-1.json")))
	evidenceFile(t, quarantine, supplementTemp+"interrupted", 8, 2*supplementTempAge)
	c := coldStart(t, dir)
	if names := supplementEvidenceNames(t, dir); len(names) != 0 {
		t.Fatalf("restart did not finish the interrupted collection: %v", names)
	}
	var wg sync.WaitGroup
	for n := range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			record := supplementRecord{Version: 1, Workspace: testID, Runtime: testID, Task: testID, Dispatched: fmt.Sprint(n), Current: &taskSupplement{CommentID: testID, Content: "generic guidance"}}
			if err := c.supplements.quarantine(record, "receipt_none"); err != nil {
				t.Error(err)
			}
			if err := c.supplements.save(record); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, _, err := c.supplements.collect(time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if names := supplementEvidenceNames(t, dir); len(names) != 32 {
		t.Fatalf("concurrent collection lost fresh evidence: %d", len(names))
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 33 {
		t.Fatalf("concurrent collection lost active journals: %d %v", len(entries), err)
	}
}
