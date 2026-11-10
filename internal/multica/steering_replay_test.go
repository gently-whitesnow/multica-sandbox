package multica

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func restartSupplementClient(t *testing.T, c *Client, dir string) *Client {
	t.Helper()
	restarted, err := New(c.base, "controller_fixture_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.UseSupplements(dir); err != nil {
		t.Fatal(err)
	}
	return restarted
}

func TestSupplementConsumptionReplaysBeforeTerminalAfterRestart(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	f.ackFailures = 1
	calls := 0
	_, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { calls++; return nil })
	if err == nil {
		t.Fatal("expected ack outage")
	}
	restarted := restartSupplementClient(t, c, dir)
	restarted.supplements.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err = restarted.UseOutbox(filepath.Join(t.TempDir(), "reports")); err != nil {
		t.Fatal(err)
	}
	if err = restarted.outbox.save(pending{Version: 1, Terminal: Terminal{Task: f.task.ID}}); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.events = nil })
	if n, err := restarted.Replay(context.Background()); err != nil || n != 2 {
		t.Fatalf("replay=%d, %v", n, err)
	}
	if !slices.Equal(f.eventLog(), []string{"ack", "complete"}) || calls != 1 || f.claimCount() != 1 {
		t.Fatal("restart reordered acknowledgement/terminal or reinjected input")
	}
	if _, err = restarted.Replay(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, fileName(f.task.AttemptKey()))); !os.IsNotExist(err) {
		t.Fatal("terminal journal retained guidance payload")
	}
}

func TestSupplementRestartNeverInjectsUnconsumedGuidance(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return errors.New("admission uncertain") })
	restarted := restartSupplementClient(t, c, dir)
	if n, err := restarted.Replay(context.Background()); n != 0 || err != nil || f.ackCount() != 0 || f.claimCount() != 1 {
		t.Fatal("restart replayed unproven consumption", err)
	}
	f.mu.Lock()
	f.status = "failed"
	f.mu.Unlock()
	if n, err := restarted.Replay(context.Background()); n != 0 || err != nil || f.ackCount() != 0 {
		t.Fatal("terminal recovery invented a delivery receipt", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName(f.task.AttemptKey()))); !os.IsNotExist(err) {
		t.Fatal("settled failed journal not collected")
	}
}

func TestSupplementSessionAttemptAndServerFences(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return errors.New("unanswered provider admission") })
	forbidden := func(context.Context, string, string) error {
		t.Error("guidance crossed session/attempt/server")
		return nil
	}
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_other", forbidden); err == nil {
		t.Fatal("same attempt accepted a different session")
	}
	stale := f.task
	stale.DispatchedAt = "2000-01-01T00:00:00Z"
	c.supplements.negotiate(stale, true)
	if _, err := c.ProcessSupplement(context.Background(), stale, "ses_fixture", forbidden); !errors.Is(err, ErrSupplementUncertain) {
		t.Fatal("different dispatch used the old task's receipt", err)
	}
	other := restartSupplementClient(t, c, dir)
	other.base = "https://other.example.invalid"
	other.supplements.negotiate(f.task, true)
	if _, err := other.ProcessSupplement(context.Background(), f.task, "ses_fixture", forbidden); err == nil {
		t.Fatal("journal accepted a different Multica origin")
	}
	if f.claimCount() != 1 || f.ackCount() != 0 {
		t.Fatal("fence mutated native delivery state")
	}
}

func TestSupplementCancellationStopsBeforeClaimAndRetiresAtTerminal(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	forbidden := func(context.Context, string, string) error { t.Error("cancelled guidance injected"); return nil }
	if _, err := c.ProcessSupplement(ctx, f.task, "ses_fixture", forbidden); !errors.Is(err, context.Canceled) || f.claimCount() != 0 {
		t.Fatal("cancellation did not fence the claim")
	}
	_, _ = c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return context.Canceled })
	f.mu.Lock()
	f.status = "cancelled"
	f.mu.Unlock()
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", forbidden); !errors.Is(err, ErrSupplementEnded) || c.SupplementsNegotiated(f.task) {
		t.Fatal("terminal task retained admission", err)
	}
	if _, err := c.ReplaySupplementAcks(context.Background()); err != nil || f.ackCount() != 0 {
		t.Fatal("cancelled unconsumed input was acknowledged", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName(f.task.AttemptKey()))); !os.IsNotExist(err) {
		t.Fatal("cancelled journal was not collected")
	}
}

func TestSupplementDiskFailurePreventsClaim(t *testing.T) {
	f, c, dir := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { t.Error("unpersisted delivery"); return nil }); err == nil || f.claimCount() != 0 {
		t.Fatal("native claim preceded durable intent")
	}
}

func TestSupplementFencedReplayChecksSubsecondGeneration(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	f.task.DispatchedAt = "2026-11-10T00:00:00.123456Z"
	startSupplementFixture(t, f, c)
	stale := f.task
	stale.DispatchedAt = "2026-11-10T00:00:00.654321Z"
	c.supplements.negotiate(stale, true)
	if _, err := c.ProcessSupplement(context.Background(), stale, "ses_fixture", nilDelivery(t)); !errors.Is(err, ErrSupplementUncertain) || !Conflict(err) {
		t.Fatal("public second-precision history bypassed the native generation fence", err)
	}
	// The stale attempt's claim stays a native receipt; the exact generation adopts it.
	if progress, err := c.ProcessSupplement(context.Background(), f.task, "ses_fixture", func(context.Context, string, string) error { return nil }); err != nil || !progress || f.claimCount() != 1 || f.ackCount() != 1 {
		t.Fatal("exact native generation did not adopt the fenced claim", err)
	}
}

func TestSupplementClaimedRetryRechecksNativeAttemptFence(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	startSupplementFixture(t, f, c)
	original := f.task
	_, _ = c.ProcessSupplement(context.Background(), original, "ses_fixture", func(context.Context, string, string) error { return errors.New("provider admission unanswered") })
	f.mu.Lock()
	f.task.DispatchedAt = "2026-11-10T00:00:00.000001Z"
	f.mu.Unlock()
	if _, err := c.ProcessSupplement(context.Background(), original, "ses_fixture", func(context.Context, string, string) error {
		t.Error("claimed input crossed native generation")
		return nil
	}); !Conflict(err) || f.ackCount() != 0 || f.claimCount() != 1 {
		t.Fatal("claimed retry did not recheck the exact native attempt", err)
	}
}
