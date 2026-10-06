package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

type inferenceFaultWorkloads struct {
	opencode.Workloads
	mode  string
	ready <-chan struct{}
}

func (w inferenceFaultWorkloads) Start(ctx context.Context, key string) (execution.ProjectedRun, error) {
	run, err := w.Workloads.Start(ctx, key)
	if err != nil {
		return nil, err
	}
	return inferenceFaultRun{ProjectedRun: run, mode: w.mode, ready: w.ready}, nil
}

type inferenceFaultRun struct {
	execution.ProjectedRun
	mode  string
	ready <-chan struct{}
}

func (r inferenceFaultRun) Write(ctx context.Context, path string, data []byte) error {
	if path == opencode.InferencePath && r.mode != "gateway" {
		var projected map[string]any
		if json.Unmarshal(data, &projected) != nil {
			return fmt.Errorf("invalid inference projection")
		}
		if r.mode == "missing" {
			delete(projected, "access_token")
		} else {
			projected["expires_at"] = time.Now().Add(-time.Second).Unix()
		}
		data, _ = json.Marshal(projected)
	}
	return r.ProjectedRun.Write(ctx, path, data)
}
func inferenceFaults(ctx context.Context, base *opencode.Adapter, states *rotationStatus, task multica.Task) error {
	for i, mode := range []string{"missing", "expired", "gateway"} {
		before, err := inferenceRequestCount(ctx)
		if err != nil {
			return err
		}
		adapter := *base
		var ready chan struct{}
		if mode == "gateway" {
			ready = make(chan struct{})
		}
		adapter.Workloads = inferenceFaultWorkloads{Workloads: base.Workloads, mode: mode, ready: ready}
		task.ID = fmt.Sprintf("40000000-0000-4000-8000-%012d", i+40)
		task.DispatchedAt = time.Now().UTC().Format(time.RFC3339Nano)
		states.set(task.ID, "running")
		run, err := adapter.Start(ctx, task)
		if err != nil {
			return err
		}
		if mode == "gateway" {
			if exec.CommandContext(ctx, "docker", "stop", os.Getenv("INFERENCE_PEER")).Run() != nil {
				return fmt.Errorf("fixture gateway stop failed")
			}
			close(ready)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if mode == "gateway" {
			// Native OpenCode owns transient provider retries; cancellation bounds this outage test.
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
			states.set(task.ID, "cancelled")
		}
		failure := run.Wait(waitCtx)
		timedOut := waitCtx.Err() != nil
		cancel()
		if mode == "gateway" {
			if exec.CommandContext(ctx, "docker", "start", os.Getenv("INFERENCE_PEER")).Run() != nil {
				return fmt.Errorf("fixture gateway restart failed")
			}
		}
		cleanup := run.Remove(context.Background())
		if cleanup != nil {
			return cleanup
		}
		if failure == nil || timedOut {
			return fmt.Errorf("%s inference failure reported success", mode)
		}
		after, err := inferenceRequestCount(ctx)
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("%s identity sent a gateway authorization request", mode)
		}
		if err := noInferenceRequests(ctx, task); err != nil {
			return err
		}
		fmt.Println("PASS native inference", mode, "failure has no successful gateway admission or credential fallback")
	}
	return nil
}
func noInferenceRequests(ctx context.Context, task multica.Task) error {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	status, body, err := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
	if err != nil || status != 200 {
		return fmt.Errorf("fault evidence unavailable")
	}
	var counts map[string][2]int
	if json.Unmarshal(body, &counts) != nil {
		return fmt.Errorf("invalid fault evidence")
	}
	for key, value := range counts {
		if strings.HasPrefix(key, "inference:") && strings.Contains(key, task.ID) && value[0] != 0 {
			return fmt.Errorf("failed identity admitted inference")
		}
	}
	return nil
}

func inferenceRequestCount(ctx context.Context) (int, error) {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return 0, err
	}
	status, body, err := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
	if err != nil || status != 200 {
		return 0, fmt.Errorf("request evidence unavailable")
	}
	var counts map[string][2]int
	if json.Unmarshal(body, &counts) != nil {
		return 0, fmt.Errorf("invalid request evidence")
	}
	return counts["inference-requests"][0], nil
}

func (r inferenceFaultRun) Execute(ctx context.Context, args []string) error {
	if r.ready != nil && len(args) >= 3 && strings.HasPrefix(args[2], "exec opencode run") {
		select {
		case <-r.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.ProjectedRun.Execute(ctx, args)
}

func (r inferenceFaultRun) Stream(ctx context.Context, args []string, env map[string]string, consume func(io.Reader) error) error {
	if r.ready != nil {
		select {
		case <-r.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.ProjectedRun.Stream(ctx, args, env, consume)
}
