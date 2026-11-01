//go:build upstream

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// A transcript batch resent after a lost answer is stored once when its rows carry
// idempotency keys; without them Multica stores every copy.
func messageReplay(t *testing.T, api *multica.Client, rt multica.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := enqueue(t, rt, 60)
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("claim: %v", err)
	}
	if err = api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	keyed := []multica.Message{{Seq: 2, Type: "text", Content: "kept once", CreatedAt: now, IdempotencyKey: "attempt:2"}}
	bare := []multica.Message{{Seq: 3, Type: "text", Content: "stored twice", CreatedAt: now}}
	for range 2 {
		if err = api.ReportMessages(ctx, id, keyed); err != nil {
			t.Fatal(err)
		}
		if err = api.ReportMessages(ctx, id, bare); err != nil {
			t.Fatal(err)
		}
	}
	counts := sql(t, fmt.Sprintf("SELECT string_agg(seq || ':' || n, ',' ORDER BY seq) FROM (SELECT seq, count(*) n FROM task_message WHERE task_id='%s' AND seq > 1 GROUP BY seq) s;", id))
	if counts != "2:1,3:2" {
		t.Fatalf("replayed rows per seq: %s", counts)
	}
	if err = api.Complete(ctx, id, execution.Result{}); err != nil {
		t.Fatal(err)
	}
	t.Log("keyed transcript replay stored once; unkeyed replay duplicates")
}
