//go:build upstream

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func nativeSupplementAckQuarantine(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 83, "opencode", true, true)
	client, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = client.UseSupplements(dir); err != nil {
		t.Fatal(err)
	}
	if err = client.Start(context.Background(), s.task); err != nil {
		t.Fatal(err)
	}
	comment := s.create(1, "terminal acknowledgement evidence fixture", 201)
	_, err = client.ProcessSupplement(context.Background(), s.task, "ses_ackfixture", func(context.Context, string, string) error {
		// This receipt was claimed, but deletion removes the SQL acknowledgement
		// target. Completion cannot turn its rejection into a delivered receipt.
		sql(t, fmt.Sprintf("DELETE FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", s.task.ID, comment))
		s.finish()
		return nil
	})
	if err != nil || client.SupplementsNegotiated(s.task) {
		t.Fatal("native terminal 409 was not settled", err)
	}
	if active := steeringJournals(t, dir); len(active) != 0 {
		t.Fatalf("refused consumption stayed in the replay set: %v", active)
	}
	data := steeringEvidence(t, dir, 1)
	if !strings.Contains(data, `"phase":"consumed"`) || !strings.Contains(data, `"reason":"receipt_none"`) || !strings.Contains(data, comment) || strings.Contains(data, token(t)) {
		t.Fatal("quarantine lost evidence or persisted credentials")
	}
	restarted, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.UseSupplements(dir); err != nil {
		t.Fatal(err)
	}
	if n, err := restarted.ReplaySupplementAcks(context.Background()); n != 0 || err != nil {
		t.Fatal("native quarantine was automatically acknowledged again", err)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE issue_id='%s';", s.task.IssueID)); got != "1" {
		t.Fatal("ack refusal spawned another native task")
	}
}

// steeringJournals lists the active (replayable) journal files of a client.
func steeringJournals(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	return names
}

// steeringEvidence returns the concatenated quarantined evidence after checking its count.
func steeringEvidence(t *testing.T, dir string, want int) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "quarantine"))
	if err != nil && !(os.IsNotExist(err) && want == 0) {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("quarantined evidence entries=%d, want %d", len(entries), want)
	}
	var all strings.Builder
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "quarantine", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
	}
	return all.String()
}
