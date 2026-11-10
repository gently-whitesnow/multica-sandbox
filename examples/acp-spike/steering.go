package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// steeringTurn establishes an active tool before probing a second prompt. A
// timeout is inconclusive, not a provider rejection or an acknowledgement.
func (s *spike) steeringTurn(ctx context.Context, a *agent, id acp.SessionId) error {
	type result struct {
		response acp.PromptResponse
		err      error
	}
	original := make(chan result, 1)
	go func() {
		response, err := a.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markCancel)}})
		original <- result{response, err}
	}()
	if !a.client.waitStatus(ctx, "in_progress", 30*time.Second) {
		return fmt.Errorf("steering probe: original prompt never reached an active tool")
	}
	probe, end := context.WithTimeout(ctx, 3*time.Second)
	defer end()
	_, err := a.conn.Prompt(probe, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(steeringSentinel)}})
	var rpc *acp.RequestError
	rejected := errors.As(err, &rpc)
	active := true
	select {
	case <-original:
		active = false
	default:
	}
	s.check("concurrent prompt rejected", rejected && active, "RPC rejection=%v; original still active=%v; error=%v", rejected, active, err)
	if err := a.conn.Cancel(ctx, acp.CancelNotification{SessionId: id}); err != nil {
		return fmt.Errorf("steering probe cancel: %w", err)
	}
	if active {
		select {
		case result := <-original:
			if result.err != nil || result.response.StopReason != acp.StopReasonCancelled {
				return fmt.Errorf("steering probe: original cancellation response: %s, %v", result.response.StopReason, result.err)
			}
		case <-time.After(15 * time.Second):
			return fmt.Errorf("steering probe: original prompt did not answer cancellation")
		}
	}
	s.check("no additional model input", !steeringObserved.Load(), "sentinel reached inference=%v", steeringObserved.Load())
	if !rejected || !active || steeringObserved.Load() {
		return fmt.Errorf("steering probe: concurrent prompt behavior needs investigation; no live-input support inferred")
	}
	return nil
}
