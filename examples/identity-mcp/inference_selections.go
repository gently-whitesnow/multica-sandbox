package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

type staleCatalogSource struct{ opencode.Inference }

func (s staleCatalogSource) Catalog(context.Context, identity.Ref) (inference.Catalog, error) {
	return inference.Catalog{}, inference.ErrDenied
}
func inferenceSelections(ctx context.Context, base *opencode.Adapter, states *rotationStatus, task multica.Task) error {
	for i, model := range []string{"fixture-new", "ungranted", "fixture-limited"} {
		adapter := *base
		adapter.Inference = staleCatalogSource{base.Inference}
		copyAgent := *task.Agent
		copyAgent.Model = "managed-inference/" + model
		copyAgent.ThinkingLevel = "high"
		copyAgent.Instructions = "Reply briefly."
		copyAgent.MCPConfig = json.RawMessage(`{}`)
		task.Agent = &copyAgent
		task.ID = fmt.Sprintf("40000000-0000-4000-8000-%012d", i+70)
		task.DispatchedAt = time.Now().UTC().Format(time.RFC3339Nano)
		states.set(task.ID, "running")
		run, err := adapter.Start(ctx, task)
		if err != nil {
			return fmt.Errorf("explicit selection denied before gateway: %w", err)
		}
		waitTime := 30 * time.Second
		if model == "fixture-limited" {
			waitTime = 8 * time.Second
		}
		waitCtx, cancel := context.WithTimeout(ctx, waitTime)
		err = run.Wait(waitCtx)
		cancel()
		cleanErr := run.Remove(context.Background())
		if cleanErr != nil {
			return cleanErr
		}
		switch model {
		case "fixture-new":
			if err != nil {
				return fmt.Errorf("stale-catalog selection failed: %w", err)
			}
		case "ungranted":
			var denied *execution.AgentFailure
			if !errors.As(err, &denied) || denied.Status != 403 {
				return fmt.Errorf("model refusal not surfaced: %v", err)
			}
		case "fixture-limited":
			if err == nil {
				return fmt.Errorf("gateway budget refusal completed task")
			}
		}
		secret, e := os.ReadFile("/secrets/admin")
		if e != nil {
			return e
		}
		status, data, e := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
		var evidence map[string][2]int
		if e != nil || status != 200 || json.Unmarshal(data, &evidence) != nil {
			return fmt.Errorf("selection evidence unavailable")
		}
		requested := false
		for key, count := range evidence {
			if !strings.HasPrefix(key, "selection:"+task.AttemptKey()+"|") {
				continue
			}
			if key != "selection:"+task.AttemptKey()+"|"+model+"|high" || count[0] < 1 {
				return fmt.Errorf("model/effort substituted: %s", model)
			}
			requested = true
		}
		if !requested {
			return fmt.Errorf("selected model/effort did not reach gateway: %s", model)
		}
	}
	fmt.Println("PASS native model/reasoning selection survives catalog outage; gateway model/budget refusals preserve selection without substitution")
	return nil
}
