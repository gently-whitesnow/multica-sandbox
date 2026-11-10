package multica

import (
	"sync"
	"testing"
)

func TestSupplementWakeupScopeCoalescingAndRetirement(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	if c.SupplementUpdates(f.task) != nil {
		t.Fatal("unnegotiated attempt subscribed")
	}
	startSupplementFixture(t, f, c)
	updates := c.SupplementUpdates(f.task)
	if updates == nil || c.SupplementUpdates(f.task) != updates {
		t.Fatal("attempt hint channel not stable")
	}
	c.WakeSupplement(testID, f.task.ID)
	c.WakeSupplement(f.task.RuntimeID, testID+"invalid")
	c.WakeSupplement(f.task.RuntimeID, f.task.IssueID)
	select {
	case <-updates:
		t.Fatal("foreign runtime/task woke attempt")
	default:
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); c.WakeSupplement(f.task.RuntimeID, f.task.ID) }()
	}
	wg.Wait()
	select {
	case <-updates:
	default:
		t.Fatal("own hint lost")
	}
	select {
	case <-updates:
		t.Fatal("hints were not coalesced")
	default:
	}
	c.supplements.retire(f.task.ID)
	c.WakeSupplement(f.task.RuntimeID, f.task.ID)
	if c.SupplementUpdates(f.task) != nil {
		t.Fatal("retired attempt remained subscribed")
	}
	select {
	case <-updates:
		t.Fatal("retired worker received a hint/closed channel")
	default:
	}
	next := f.task
	next.DispatchedAt = "2026-11-10T00:00:01Z"
	c.supplements.negotiate(next, true)
	fresh := c.SupplementUpdates(next)
	if fresh == nil || fresh == updates || c.SupplementUpdates(f.task) != nil {
		t.Fatal("attempt generation reused prior hint channel")
	}
	c.WakeSupplement(next.RuntimeID, next.ID)
	select {
	case <-fresh:
	default:
		t.Fatal("next attempt did not get its own hint")
	}
	select {
	case <-updates:
		t.Fatal("new attempt woke retired worker")
	default:
	}
}

func TestSupplementWakeupUnavailableWithoutSteering(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	c.supplements = nil
	c.WakeSupplement(f.task.RuntimeID, f.task.ID)
	if c.SupplementUpdates(f.task) != nil {
		t.Fatal("disabled steering exposes a hint channel")
	}
}
