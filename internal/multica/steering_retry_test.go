package multica

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nilDelivery(t *testing.T) func(context.Context, string, string) error {
	return func(context.Context, string, string) error { t.Error("unexpected delivery"); return nil }
}

func supplementEvidenceNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, supplementEvidenceDir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestSupplementDefinitiveClaimRejectionAllowsRetry(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 412, 429} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.claimStatus = code
			calls := 0
			deliver := func(context.Context, string, string) error { calls++; return nil }
			if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); !statusIs(err, code) {
				t.Fatalf("claim rejection lost: %v", err)
			}
			if supplementPhase(t, c, f.task, dir).Phase != "idle" || calls != 0 {
				t.Fatal("definitively rejected claim retained ambiguous intent or injected guidance")
			}
			if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); progress || err != nil || f.claimCount() != 1 {
				t.Fatal("definitive rejection was retried without backoff", err)
			}
			f.set(func() { f.claimStatus = 0 })
			c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
			if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); !progress || err != nil || calls != 1 || f.claimCount() != 2 {
				t.Fatal("server refusal permanently stalled the pending input", err)
			}
		})
	}
}

func TestSupplementAmbiguousClaimStatusBacksOffThenRecovers(t *testing.T) {
	for _, code := range []int{408, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.claimStatus = code
			calls := 0
			deliver := func(context.Context, string, string) error { calls++; return nil }
			_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver)
			for range 3 {
				if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); progress || err != nil {
					t.Fatalf("ambiguous request was retried inside its backoff: %v", err)
				}
			}
			if f.claimCount() != 1 || calls != 0 || supplementPhase(t, c, f.task, dir).Phase != "claiming" {
				t.Fatal("claim ambiguity advanced or lost its intent")
			}
			f.set(func() { f.claimStatus = 0 })
			c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
			if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); !progress || err != nil || calls != 1 {
				t.Fatal("ambiguous claim never recovered", err)
			}
		})
	}
}

func TestSupplementRunningPermanentAckRetainsConsumption(t *testing.T) {
	// 409 with a receipt that is still deliverable is not a statement about it.
	for _, code := range []int{400, 401, 403, 409} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.ackStatus = code
			_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return nil })
			if !statusIs(err, code) || supplementPhase(t, c, f.task, dir).Phase != "consumed" || !c.SupplementsNegotiated(f.task) {
				t.Fatal("running receipt was discarded after a permanent HTTP error", err)
			}
			for range 5 {
				if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); err != nil {
					t.Fatal("permanent ack error busy-looped", err)
				}
			}
			if f.ackCount() != 1 || len(supplementEvidenceNames(t, dir)) != 0 {
				t.Fatal("running ack was resent without backoff or quarantined without proof")
			}
			f.set(func() { f.ackStatus = 0 })
			c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
			if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); !progress || err != nil || f.ackCount() != 2 {
				t.Fatal("retained consumption was not acknowledged after recovery", err)
			}
		})
	}
}

// Deleting a claimed comment removes its native receipt. The consumed text is
// kept as evidence, never called delivered, and later input keeps flowing.
func TestSupplementDeletedReceiptDoesNotBlockLaterInput(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	texts := []string{}
	deleted := f.rows[0].Message.CommentID
	progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(_ context.Context, _, text string) error {
		texts = append(texts, text)
		f.remove(deleted)
		return nil
	})
	if !progress || err != nil || !c.SupplementsNegotiated(f.task) || supplementPhase(t, c, f.task, dir).Phase != "idle" {
		t.Fatal("deleted receipt stalled the attempt", err)
	}
	names := supplementEvidenceNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("consumption evidence: %v", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, supplementEvidenceDir, names[0]))
	if err != nil || !strings.Contains(string(data), `"reason":"receipt_none"`) || !strings.Contains(string(data), "guidance 0") || strings.Contains(string(data), "controller_fixture_secret") {
		t.Fatal("evidence lost its reason or guidance, or holds credentials", err)
	}
	deliver := func(_ context.Context, _, text string) error { texts = append(texts, text); return nil }
	if progress, err = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); !progress || err != nil {
		t.Fatal("later input was not delivered after a deleted receipt", err)
	}
	if len(texts) != 2 || !strings.Contains(texts[1], "guidance 1") || f.ackCount() != 2 || f.rows[0].Status != "delivered" {
		t.Fatal("later input lost its order or receipt")
	}
	restarted := restartSupplementClient(t, c, dir)
	if n, err := restarted.ReplaySupplementAcks(context.Background()); n != 0 || err != nil || f.ackCount() != 2 {
		t.Fatal("quarantined evidence was replayed", err)
	}
}

func TestSupplementDeletedClaimBeforeConsumptionIsDropped(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return errors.New("not consumed") })
	f.remove(f.rows[0].Message.CommentID)
	texts := []string{}
	deliver := func(_ context.Context, _, text string) error { texts = append(texts, text); return nil }
	for range 2 {
		if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err != nil {
			t.Fatal(err)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "guidance 1") || f.ackCount() != 1 || len(supplementEvidenceNames(t, dir)) != 0 {
		t.Fatal("deleted unconsumed claim was injected, acknowledged or blocked later input")
	}
}

func TestSupplementUndeliverableClaimIsRejectedNatively(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.rows[0].Message.Content = strings.Repeat("x", 64<<10+1)
	texts := []string{}
	deliver := func(_ context.Context, _, text string) error { texts = append(texts, text); return nil }
	for range 2 {
		if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); err != nil {
			t.Fatal(err)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "guidance 1") || f.count(&f.nacks) != 1 || f.rows[0].Status != "rejected" {
		t.Fatal("undeliverable claim stalled the queue or was injected")
	}
}

func TestSupplementInspectionOutageDefersWithoutLosingState(t *testing.T) {
	for _, code := range []int{404, 405, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.ackFailures = 1
			_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return nil })
			f.set(func() { f.inspectStatus, f.ackStatus = code, 409 })
			c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
			if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); err == nil {
				t.Fatal("unreadable receipt state was treated as an answer")
			}
			if supplementPhase(t, c, f.task, dir).Phase != "consumed" || !c.SupplementsNegotiated(f.task) || len(supplementEvidenceNames(t, dir)) != 0 {
				t.Fatal("inspection outage discarded or quarantined consumption")
			}
		})
	}
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.set(func() { f.inspectStatus = 404 })
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); !errors.Is(err, ErrSupplementUncertain) || !c.SupplementsNegotiated(f.task) || f.claimCount() != 0 {
		t.Fatal("missing inspection endpoint was read as a missing task or claimed blindly", err)
	}
	now := time.Now()
	c.supplements.now = func() time.Time { return now }
	for _, body := range []string{`{}`, `{"task_status":"running","receipt":{"comment_id":"x","status":"none"}}`, `{"task_status":"running","delivering":{"comment_id":"not-an-id"}}`, `{"task_status":"paused"}`} {
		now = now.Add(time.Hour)
		f.set(func() { f.inspectStatus, f.inspectBody = 0, body })
		if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); !errors.Is(err, ErrSupplementUncertain) || f.claimCount() != 0 {
			t.Fatalf("invalid inspection %s admitted a claim: %v", body, err)
		}
	}
}

func TestSupplementReplayStatusOutageUsesPersistedBackoff(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return errors.New("unconsumed") })
	f.set(func() { f.statusCode = 503 })
	before := f.count(&f.statuses)
	if _, err := c.ReplaySupplementAcks(context.Background()); !statusIs(err, 503) {
		t.Fatal("status outage hidden", err)
	}
	restarted := restartSupplementClient(t, c, dir)
	for range 5 {
		if _, err := restarted.ReplaySupplementAcks(context.Background()); err != nil {
			t.Fatal("status retry ignored persisted delay", err)
		}
	}
	if f.count(&f.statuses) != before+1 || f.ackCount() != 0 || f.claimCount() != 1 {
		t.Fatal("outage caused repeated status calls or native writes")
	}
}

func TestSupplementAckBackoffSurvivesRestartAndNeverReinjects(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.ackStatus = 429
	now := time.Now()
	c.supplements.now = func() time.Time { return now }
	calls := 0
	deliver := func(context.Context, string, string) error { calls++; return nil }
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver)
	restarted := restartSupplementClient(t, c, dir)
	restarted.supplements.now = func() time.Time { return now }
	for n := range 10 {
		for range 5 {
			if _, err := restarted.ReplaySupplementAcks(context.Background()); err != nil {
				t.Fatal("deferred ack still contacted the server", err)
			}
		}
		if f.ackCount() != n+1 || calls != 1 || f.claimCount() != 1 {
			t.Fatal("ack retry spun or injected guidance again")
		}
		record := supplementPhase(t, restarted, f.task, dir)
		delay := time.Unix(0, record.RetryAfter).Sub(now)
		if delay <= 0 || delay > 5*time.Minute || n == 0 && delay != 4*time.Second {
			t.Fatalf("unbounded retry delay: %s", delay)
		}
		now = now.Add(delay)
		if _, err := restarted.ReplaySupplementAcks(context.Background()); !statusIs(err, 429) {
			t.Fatalf("due failure was hidden: %v", err)
		}
	}
	f.set(func() { f.ackStatus = 0 })
	now = now.Add(5 * time.Minute)
	if n, err := restarted.ReplaySupplementAcks(context.Background()); n != 1 || err != nil || calls != 1 || f.claimCount() != 1 {
		t.Fatal("outage recovery failed or reinjected", err)
	}
}

func TestSupplementGoneJournalCollected(t *testing.T) {
	for _, code := range []int{404, 410} {
		for _, consumed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/consumed=%v", code, consumed), func(t *testing.T) {
				f, c, dir := newSupplementFixture(t)
				startSupplementFixture(t, f, c)
				f.ackStatus = 503
				_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error {
					if !consumed {
						return errors.New("not consumed")
					}
					return nil
				})
				f.set(func() { f.statusCode, f.inspectStatus, f.ackStatus = code, code, code })
				c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
				if consumed {
					_, _ = c.ReplaySupplementAcks(context.Background())
				} else {
					_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t))
				}
				if _, err := os.Stat(filepath.Join(dir, fileName(f.task.AttemptKey()))); !os.IsNotExist(err) || c.SupplementsNegotiated(f.task) {
					t.Fatal("gone task retained a replaying journal/negotiation")
				}
				acks := f.ackCount()
				_, _ = c.ReplaySupplementAcks(context.Background())
				if f.ackCount() != acks {
					t.Fatal("gone journal kept retrying")
				}
			})
		}
	}
}

// A terminal task settles its receipts as failed/turn_ended, which Multica still
// acknowledges late. Any other terminal refusal is quarantined, never replayed.
func TestSupplementTerminalPermanentAckQuarantined(t *testing.T) {
	for _, code := range []int{400, 403, 409} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.ackStatus = code
			_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error {
				f.set(func() { f.status = "completed" })
				f.remove(f.rows[0].Message.CommentID)
				return nil
			})
			if err != nil || c.SupplementsNegotiated(f.task) {
				t.Fatal("terminal refusal was not settled", err)
			}
			if _, err := os.Stat(filepath.Join(dir, fileName(f.task.AttemptKey()))); !os.IsNotExist(err) {
				t.Fatal("quarantined evidence remains active")
			}
			names := supplementEvidenceNames(t, dir)
			if len(names) != 1 {
				t.Fatalf("consumption evidence discarded: %v", names)
			}
			data, err := os.ReadFile(filepath.Join(dir, supplementEvidenceDir, names[0]))
			if err != nil || !strings.Contains(string(data), `"phase":"consumed"`) {
				t.Fatal("consumption evidence unreadable", err)
			}
			restarted := restartSupplementClient(t, c, dir)
			if n, err := restarted.ReplaySupplementAcks(context.Background()); n != 0 || err != nil || f.ackCount() != 1 {
				t.Fatal("quarantine was retried", err)
			}
		})
	}
}

func TestSupplementLateAckAfterTurnEndedIsRetriedNotQuarantined(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.ackStatus = 409
	_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error {
		f.set(func() { f.status, f.rows[0].Status = "completed", "failed" })
		return nil
	})
	if !statusIs(err, 409) || supplementPhase(t, c, f.task, dir).Phase != "consumed" || len(supplementEvidenceNames(t, dir)) != 0 {
		t.Fatal("a natively acknowledgeable late receipt was quarantined", err)
	}
}

func TestSupplementRecoveredClaimResetsAckBackoff(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	now := time.Now()
	c.supplements.now = func() time.Time { return now }
	f.lostClaim = true
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); err == nil {
		t.Fatal("lost claim response hidden")
	}
	now = now.Add(5 * time.Second)
	f.ackStatus = 503
	calls := 0
	_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { calls++; return nil })
	record := supplementPhase(t, c, f.task, dir)
	if !statusIs(err, 503) || calls != 1 || record.Phase != "consumed" || record.Failures != 1 || record.RetryAfter != now.Add(4*time.Second).UnixNano() {
		t.Fatalf("successful claim recovery did not reset ack retry budget: %+v err=%v calls=%d", record, err, calls)
	}
}

func TestSupplementInvalidPayloadRejectionRemainsReplayable(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.rows[0].Message.Content, f.ackStatus = "", 503
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); !statusIs(err, 503) {
		t.Fatal("negative acknowledgement failure hidden", err)
	}
	if record := supplementPhase(t, c, f.task, dir); record.Phase != "claiming" || !record.valid(c.base) {
		t.Fatalf("undeliverable payload corrupted the journal: %+v", record)
	}
	c = restartSupplementClient(t, c, dir)
	startSupplementFixture(t, f, c)
	c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
	f.set(func() { f.ackStatus = 0 })
	if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", nilDelivery(t)); !progress || err != nil {
		t.Fatal("negative acknowledgement did not recover", err)
	}
	calls := 0
	if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { calls++; return nil }); !progress || err != nil || calls != 1 {
		t.Fatal("invalid payload blocked the next valid input", err)
	}
}

func TestSupplementQuarantineWriteFailurePreservesConsumption(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	// Inject an isolated filesystem error without consuming host/engine disk.
	path := filepath.Join(dir, supplementEvidenceDir)
	if err := os.WriteFile(path, []byte("blocked directory"), 0600); err != nil {
		t.Fatal(err)
	}
	f.ackStatus = 409
	_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error {
		f.remove(f.rows[0].Message.CommentID)
		return nil
	})
	if err == nil || supplementPhase(t, c, f.task, dir).Phase != "consumed" {
		t.Fatal("failed quarantine write discarded consumption", err)
	}
	c = restartSupplementClient(t, c, dir)
	c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := c.ReplaySupplementAcks(context.Background()); err == nil {
		t.Fatal("restart hid the filesystem failure")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c.supplements.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if n, err := c.ReplaySupplementAcks(context.Background()); n != 0 || err != nil || len(supplementEvidenceNames(t, dir)) != 1 {
		t.Fatal("filesystem recovery lost evidence or acknowledged a missing receipt", err)
	}
}

func TestSupplementReplayWakesWorkerAfterConsumedAck(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	updates := c.SupplementUpdates(f.task)
	now := time.Now()
	c.supplements.now = func() time.Time { return now }
	f.ackStatus = 503
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return nil })
	if delay, pending := c.SupplementRetryDelay(f.task, "ses_fixture"); !pending || delay != 4*time.Second {
		t.Fatalf("worker cannot schedule the durable retry: %s %v", delay, pending)
	}
	now = now.Add(5 * time.Second)
	f.set(func() { f.ackStatus = 0 })
	if n, err := c.ReplaySupplementAcks(context.Background()); n != 1 || err != nil {
		t.Fatal("consumed acknowledgement did not replay", err)
	}
	select {
	case <-updates:
	default:
		t.Fatal("replay left the worker asleep with later input pending")
	}
	if _, pending := c.SupplementRetryDelay(f.task, "ses_fixture"); pending {
		t.Fatal("settled consumption retained a retry deadline")
	}
}
