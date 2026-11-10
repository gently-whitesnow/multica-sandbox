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
			f.mu.Lock()
			f.claimStatus = 0
			f.mu.Unlock()
			c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
			if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", deliver); !progress || err != nil || calls != 1 || f.claimCount() != 2 {
				t.Fatal("server refusal permanently stalled the pending input", err)
			}
		})
	}
}

func TestSupplementRunningPermanentAckRetainsConsumption(t *testing.T) {
	for _, code := range []int{401, 403, 404, 409} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.ackStatus = code
			_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return nil })
			if !statusIs(err, code) || supplementPhase(t, c, f.task, dir).Phase != "consumed" || !c.SupplementsNegotiated(f.task) {
				t.Fatal("running receipt was discarded after a permanent HTTP error", err)
			}
			for range 5 {
				if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { t.Error("reinject"); return nil }); err != nil {
					t.Fatal("permanent ack error busy-looped", err)
				}
			}
			if f.ackCount() != 1 {
				t.Fatal("running ack was repeatedly sent without backoff")
			}
		})
	}
}

func TestSupplementEmptyMetadataDoesNotReadTimeline(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.rows, f.truncated = nil, true
	for range 5 {
		if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { t.Error("empty input injected"); return nil }); progress || err != nil {
			t.Fatal("empty receipt metadata required complete issue history", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timelines != 0 || f.claims != 0 {
		t.Fatal("empty poll downloaded timeline or claimed")
	}
}

func TestSupplementAcknowledgedMetadataDoesNotReadTimeline(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	for range 2 {
		if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	f.truncated = true
	timelines := f.timelines
	f.mu.Unlock()
	for range 5 {
		if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { t.Error("delivered input reinjected"); return nil }); progress || err != nil {
			t.Fatal("acknowledged receipts required issue history", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timelines != timelines || f.claims != 2 {
		t.Fatal("confirmed receipts were repeatedly downloaded")
	}
}

func TestSupplementReplayStatusOutageUsesPersistedBackoff(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return errors.New("unconsumed") })
	f.mu.Lock()
	f.statusCode = 503
	before := f.statuses
	f.mu.Unlock()
	if _, err := c.ReplaySupplementAcks(context.Background()); !statusIs(err, 503) {
		t.Fatal("status outage hidden", err)
	}
	restarted := restartSupplementClient(t, c, dir)
	for range 5 {
		if _, err := restarted.ReplaySupplementAcks(context.Background()); err != nil {
			t.Fatal("status retry ignored persisted delay", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statuses != before+1 || f.acks != 0 || f.claims != 1 {
		t.Fatal("outage caused repeated status calls or native writes")
	}
}

func TestSupplementAmbiguousClaimStatusRetainsIntent(t *testing.T) {
	for _, code := range []int{408, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.claimStatus = code
			forbidden := func(context.Context, string, string) error { t.Error("ambiguous input injected"); return nil }
			_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", forbidden)
			for range 3 {
				if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", forbidden); !errors.Is(err, ErrSupplementUncertain) {
					t.Fatalf("ambiguous request was retried: %v", err)
				}
			}
			if f.claimCount() != 1 || supplementPhase(t, c, f.task, dir).Phase != "claiming" {
				t.Fatal("claim ambiguity advanced to another input")
			}
		})
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
	f.mu.Lock()
	f.ackStatus = 0
	f.mu.Unlock()
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
				f.ackStatus = 409
				_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error {
					if !consumed {
						return errors.New("not consumed")
					}
					return nil
				})
				f.mu.Lock()
				f.statusCode = code
				f.mu.Unlock()
				c.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
				if consumed {
					_, _ = c.ReplaySupplementAcks(context.Background())
				} else {
					_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { t.Error("gone task injected"); return nil })
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

func TestSupplementTerminalPermanentAckQuarantined(t *testing.T) {
	for _, code := range []int{400, 403, 404, 409} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, c, dir := newSupplementFixture(t)
			startSupplementFixture(t, f, c)
			f.ackStatus = code
			_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error {
				f.mu.Lock()
				f.status = "completed"
				f.mu.Unlock()
				return nil
			})
			if !statusIs(err, code) || c.SupplementsNegotiated(f.task) {
				t.Fatal("permanent refusal was hidden", err)
			}
			path := filepath.Join(dir, fileName(f.task.AttemptKey()))
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("quarantined evidence remains active")
			}
			data, err := os.ReadFile(path + ".quarantine")
			if err != nil || !strings.Contains(string(data), `"phase":"consumed"`) {
				t.Fatal("consumption evidence discarded", err)
			}
			restarted := restartSupplementClient(t, c, dir)
			if n, err := restarted.ReplaySupplementAcks(context.Background()); n != 0 || err != nil || f.ackCount() != 1 {
				t.Fatal("quarantine was retried", err)
			}
		})
	}
}
