//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func nativeSteeringAdmission(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_STEERING_NATIVE_PATCH") != "1" {
		t.Skip("requires the opt-in disposable native patches")
	}
	s := supplementAdmissionFixture(t, api, 79, "opencode", true, true)
	if got := sql(t, fmt.Sprintf("SELECT provider FROM agent_runtime WHERE id='%s';", s.task.RuntimeID)); got != "opencode" {
		t.Fatalf("native admission fixture impersonates provider %q", got)
	}
	direct := s.create(1, "direct native input", 201)
	response := s.request("POST", "/api/issues/"+s.task.IssueID+"/comments", fmt.Sprintf(`{"content":"[@Fixture](mention://agent/%s) composer input","steer_task_ids":[%q]}`, s.task.AgentID, s.task.ID), 201)
	var comment struct {
		ID       string
		Outcomes []struct{ Status string } `json:"trigger_outcomes"`
	}
	if json.Unmarshal([]byte(response), &comment) != nil || len(comment.Outcomes) != 1 || comment.Outcomes[0].Status != "steered" {
		t.Fatalf("native composer did not bind to the active OpenCode run: %s", response)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id='%s';", comment.ID)); got != "0" {
		t.Fatal("steered comment created a follow-up task")
	}
	for _, id := range []string{direct, comment.ID} {
		if got := s.claim(); got != id {
			t.Fatalf("native ordered claim=%s, want %s", got, id)
		}
		s.ack(id, true, "", 200)
	}
	s.finish()
	missing := supplementAdmissionFixture(t, api, 80, "opencode", false, false)
	missing.create(1, "no capability", 412)
	missing.finish()
	t.Run("client-journal", func(t *testing.T) { nativeSupplementJournal(t, api) })
	t.Run("client-terminal-ack-quarantine", func(t *testing.T) { nativeSupplementAckQuarantine(t, api) })
	t.Run("receipt-inspection", func(t *testing.T) { nativeReceiptInspection(t, api) })
	t.Run("client-large-history", func(t *testing.T) { nativeSteeringLargeHistory(t, api) })
	t.Run("client-vanished-receipt", func(t *testing.T) { nativeSteeringVanishedReceipt(t, api) })
	t.Run("client-lost-responses", func(t *testing.T) { nativeSteeringLostResponses(t, api) })
	t.Run("client-cancel-race", func(t *testing.T) { nativeSteeringCancelRace(t, api) })
	t.Log("fixture-only patches enable native OpenCode negotiation, direct creation, composer binding and bounded receipt inspection; no agent delivery is asserted")
}

func nativeSupplementJournal(t *testing.T, api *multica.Client) {
	s := supplementAdmissionFixture(t, api, 82, "opencode", true, true)
	client, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.UseSupplements(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err = client.Start(context.Background(), s.task); err != nil || !client.SupplementsNegotiated(s.task) {
		t.Fatalf("native journal negotiation: %v", err)
	}
	comments := []string{s.create(1, "first journal fixture input", 201), s.create(2, "second journal fixture input", 201)}
	ids := []string{}
	for n, comment := range comments {
		progress, err := client.ProcessSupplement(context.Background(), s.task, "ses_nativejournal", func(ctx context.Context, id, text string) error {
			if !strings.Contains(text, []string{"first journal", "second journal"}[n]) {
				t.Fatal("native claim order changed")
			}
			status, err := api.Status(ctx, s.task.ID)
			if err != nil || status != "running" {
				t.Fatal("fixture callback did not run inside the original native run")
			}
			ids = append(ids, id)
			return nil // Deterministic consumption callback, not an OpenCode model.
		})
		if err != nil || !progress {
			t.Fatalf("real native journal delivery: %v", err)
		}
		s.receipt(comment, "delivered||1")
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatal("native journal lost per-message identity")
	}
	if progress, err := client.ProcessSupplement(context.Background(), s.task, "ses_nativejournal", func(context.Context, string, string) error {
		t.Fatal("empty native queue reinjected input")
		return nil
	}); err != nil || progress {
		t.Fatalf("empty native journal: %v", err)
	}
	s.finish()
	if _, err = client.ReplaySupplementAcks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.SupplementsNegotiated(s.task) {
		t.Fatal("externally completed task retained steering capability")
	}
}
