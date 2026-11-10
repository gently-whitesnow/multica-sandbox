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
	if !multica.Conflict(err) || client.SupplementsNegotiated(s.task) {
		t.Fatal("native terminal409 was hidden or kept active", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".quarantine") {
		t.Fatal("native refusal did not preserve quarantined evidence", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || !strings.Contains(string(data), `"phase":"consumed"`) || !strings.Contains(string(data), comment) || strings.Contains(string(data), token(t)) {
		t.Fatal("quarantine lost evidence or persisted credentials", err)
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
