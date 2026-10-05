package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func inferenceFailures(ctx context.Context, base *opencode.Adapter, states *rotationStatus, task multica.Task) error {
	for i, mode := range []string{"issuer", "resolver", "cancelled"} {
		if err := inferenceFailure(ctx, base, states, task, mode, i); err != nil {
			return err
		}
	}
	return nil
}
func failureInferenceSource(mode, server string) (*recordingInference, *atomic.Bool, func(), error) {
	down := &atomic.Bool{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		if mode == "resolver" {
			var req struct {
				Agent identity.Ref `json:"agent"`
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "agent": req.Agent, "target": fixtureInferenceTarget()})
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "POST", issuer+"/protocol/openid-connect/token", r.Body)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 65536))
	}))
	c := inferenceIdentityConfig()
	if mode == "issuer" {
		c.Issuers[0].TokenURL = endpoint.URL
	}
	source, err := identity.New(c)
	if err != nil {
		endpoint.Close()
		return nil, nil, nil, err
	}
	cfg := inference.Config{Version: 1, Server: server, AllowHTTP: true, Gateways: []inference.Gateway{fixtureInferenceTarget().Gateway}, Bindings: []inference.Binding{{WorkspaceID: c.Bindings[0].WorkspaceID, AgentID: c.Bindings[0].AgentID, Target: fixtureInferenceTarget()}}}
	if mode == "resolver" {
		cfg.Bindings = nil
		cfg.External = &identity.ExternalConfig{URL: endpoint.URL, BearerFile: "/secrets/admin"}
	}
	service, err := inference.New(cfg, source)
	if err != nil {
		endpoint.Close()
		return nil, nil, nil, err
	}
	return &recordingInference{Service: service, issued: map[string][]identity.AccessToken{}}, down, endpoint.Close, nil
}
func inferenceFailure(ctx context.Context, base *opencode.Adapter, states *rotationStatus, task multica.Task, mode string, index int) error {
	recorded, down, closeSource, err := failureInferenceSource(mode, base.Server)
	if err != nil {
		return err
	}
	defer closeSource()
	adapter := *base
	adapter.Inference = recorded
	adapter.Command = []string{"/usr/local/bin/opencode", "serve", "--hostname", "127.0.0.1"}
	task.ID = fmt.Sprintf("40000000-0000-4000-8000-%012d", index+30)
	task.DispatchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	states.set(task.ID, "running")
	run, err := adapter.Start(ctx, task)
	if err != nil {
		return err
	}
	down.Store(true)
	if mode == "cancelled" {
		states.set(task.ID, "cancelled")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	failure := run.Wait(waitCtx)
	if err := run.Remove(context.Background()); err != nil {
		return err
	}
	if failure == nil || waitCtx.Err() != nil {
		return fmt.Errorf("inference %s did not stop before expiry", mode)
	}
	recorded.Lock()
	token := recorded.issued[task.WorkspaceID][0]
	recorded.Unlock()
	if !token.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("inference failure only tested expiry")
	}
	if err := deniedInference(ctx, token.Bearer()); err != nil {
		return err
	}
	fmt.Println("PASS inference", mode, "stops native OpenCode and revokes its unexpired inference JWT")
	return nil
}
