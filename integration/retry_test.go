//go:build upstream

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func serverRetry(t *testing.T, api *multica.Client, rt multica.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := enqueue(t, rt, 8)
	sql(t, fmt.Sprintf("UPDATE agent_task_queue SET max_attempts=2 WHERE id='%s';", id))
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	recovered, err := api.Recover(ctx, rt.ID)
	if err != nil || recovered.Orphaned != 1 || recovered.Retried != 1 {
		t.Fatalf("server retry: %+v %v", recovered, err)
	}
	status(t, api, id, "failed")
	retryID := sql(t, fmt.Sprintf("SELECT id FROM agent_task_queue WHERE parent_task_id='%s';", id))
	if retryID == "" || retryID == id {
		t.Fatal("server did not create a fresh attempt")
	}
	p := controller.Probe{API: api, Duration: time.Millisecond, Interval: 10 * time.Millisecond}
	if err := p.Run(ctx, rt.ID); err != nil {
		t.Fatal(err)
	}
	status(t, api, retryID, "completed")
	t.Log("Multica created and queued a fresh retry; the probe only claimed it")
}
