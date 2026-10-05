package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func rotationFailures(ctx context.Context, base *opencode.Adapter, states *rotationStatus, tasks []multica.Task) error {
	for i, mode := range []string{"issuer", "resolver", "cancelled"} {
		if err := rotationFailure(ctx, base, states, tasks[0], mode, i); err != nil {
			return err
		}
	}
	return nil
}

func rotationFailure(ctx context.Context, base *opencode.Adapter, states *rotationStatus, task multica.Task, mode string, index int) error {
	config := resolverConfig()
	secret, err := os.ReadFile("/secrets/client")
	if err != nil {
		return err
	}
	var unavailable atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		if mode == "resolver" {
			var request struct {
				Agent identity.Ref `json:"agent"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "agent": request.Agent, "issuer": "fixture", "client_id": "example-agent", "subject": serviceSubject, "client_secret": string(secret)})
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "POST", issuer+"/protocol/openid-connect/token", r.Body)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(response.Body, 65536))
	}))
	defer endpoint.Close()
	if mode == "resolver" {
		config.Bindings = nil
		config.External = &identity.ExternalConfig{URL: endpoint.URL, BearerFile: "/secrets/admin"}
	} else {
		config.Issuers[0].TokenURL = endpoint.URL
	}
	service, err := identity.New(config)
	if err != nil {
		return err
	}
	recorded := &recordingIssuer{Service: service, issued: map[string][]identity.AccessToken{}}
	adapter := *base
	adapter.Issuer = recorded
	adapter.Command = []string{"/usr/local/bin/opencode", "serve", "--hostname", "127.0.0.1"}
	task.ID = fmt.Sprintf("40000000-0000-4000-8000-%012d", index+10)
	task.DispatchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	states.set(task.ID, "running")
	run, err := adapter.Start(ctx, task)
	if err != nil {
		return err
	}
	unavailable.Store(true)
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
		return fmt.Errorf("%s did not stop before expiry", mode)
	}
	recorded.Lock()
	token := recorded.issued[task.WorkspaceID][0]
	recorded.Unlock()
	if !token.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("revocation was only token expiry")
	}
	if err := call(ctx, token.Bearer(), task.WorkspaceID, "document", false); err != nil {
		return fmt.Errorf("%s retained authorization", mode)
	}
	fmt.Println("PASS", mode, "stops native OpenCode and denies its still-unexpired JWT after cleanup")
	return nil
}
