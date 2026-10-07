package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

type Issuer interface {
	AcquireForMCP(context.Context, identity.Ref, string) (identity.AccessToken, error)
}
type Authority interface {
	Apply(context.Context, attempt.Grant) error
}
type Workloads interface {
	Start(context.Context, string) (execution.ProjectedRun, error)
}
type Status interface {
	Status(context.Context, string) (string, error)
}

// Grants issues per-attempt relay credentials; upstream credentials stay in controller memory.
type Grants interface {
	Issue(string, relay.Upstream) (string, error)
	Revoke(string)
}

type Adapter struct {
	Server, Controller string
	Issuer             Issuer
	Inference          Inference
	Authority          Authority
	Workloads          Workloads
	Status             Status
	Command            []string
	Reporter           Reporter
	// Relay and RelayURL enable upstream-equivalent Multica CLI access (ADR 0014).
	Relay    Grants
	RelayURL string
	// InferenceRelay and InferenceRelayURL carry workspace-key inference (ADR 0012).
	InferenceRelay    Grants
	InferenceRelayURL string
}

type running struct {
	adapter     *Adapter
	task        multica.Task
	connections map[string]Remote
	workload    execution.ProjectedRun
	tokens      map[string]identity.AccessToken
	cancel      context.CancelFunc
	done        chan error
	exited      chan struct{}
	started     bool
	once        sync.Once
	cleanupErr  error
	events      *eventStream
	model       string
	relayToken  string
	// inferenceToken is the opaque per-attempt provider key; it never rotates.
	inferenceToken string
}

func (a *Adapter) Start(ctx context.Context, task multica.Task) (execution.Run, error) {
	connections, err := Select(task)
	if err != nil {
		return nil, &execution.RejectedError{Err: err}
	}
	prompt, brief, err := Prompt(task, a.Relay != nil)
	if err != nil {
		return nil, &execution.RejectedError{Err: err}
	}
	r := &running{adapter: a, task: task, connections: connections, tokens: map[string]identity.AccessToken{}, done: make(chan error, 1), exited: make(chan struct{})}
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	if a.Relay != nil {
		token, err := task.TaskToken()
		if err == nil {
			r.relayToken, err = a.Relay.Issue(task.AttemptKey(), relay.Upstream{Origin: a.Server, Credential: token})
		}
		if err != nil {
			return nil, r.reject(fmt.Errorf("Multica relay grant: %w", ErrDenied))
		}
	}
	if err = r.grantInference(runCtx); err != nil {
		cancel()
		return nil, r.reject(err)
	}
	if err = r.refresh(runCtx); err != nil {
		cancel()
		return nil, r.reject(err)
	}
	r.workload, err = a.Workloads.Start(runCtx, task.AttemptKey())
	if err != nil {
		cancel()
		return nil, errors.Join(err, r.Remove(context.Background()))
	}
	if err = r.initialize(runCtx, prompt, brief); err != nil {
		return nil, r.reject(err)
	}
	r.events = &eventStream{reporter: a.Reporter, task: task.ID, model: r.model}
	r.events.touch()
	r.started = true
	go r.loop(runCtx)
	return r, nil
}

func (r *running) initialize(ctx context.Context, prompt, brief []byte) error {
	if err := r.workload.Execute(ctx, []string{"/bin/sh", "-c", `set -eu; mkdir -p /workspace/config/opencode; printf "{}" > /workspace/config/opencode/opencode.json; printf "*\n" > /workspace/config/opencode/.gitignore; chmod 555 /workspace/config/opencode`}); err != nil {
		return fmt.Errorf("OpenCode config directory: %w", ErrDenied)
	}
	config, err := Config(r.connections, r.relayToken != "")
	if err != nil {
		return err
	}
	config, err = r.configureInference(ctx, config)
	if err != nil {
		return err
	}
	files := map[string][]byte{"/workspace/opencode.json": config, "/workspace/prompt.txt": prompt}
	if brief != nil {
		files[BriefPath] = brief
	}
	for path, data := range files {
		if err := r.workload.Write(ctx, path, data); err != nil {
			return err
		}
	}
	return r.project(ctx)
}

func (r *running) project(ctx context.Context) error {
	status, err := r.adapter.Status.Status(ctx, r.task.ID)
	if err != nil || status != "running" {
		return ErrDenied
	}
	store := map[string]Entry{}
	for name, token := range r.tokens {
		if time.Until(token.ExpiresAt) <= time.Second {
			return ErrDenied
		}
		store[name] = Entry{r.connections[name].URL, Token{token.Bearer(), token.ExpiresAt.Unix()}}
	}
	data, err := json.Marshal(store)
	if err != nil {
		return ErrDenied
	}
	return r.workload.Write(ctx, AuthPath, data)
}

func (r *running) grant(action string) attempt.Grant {
	return attempt.Grant{Controller: r.adapter.Controller, Attempt: r.task.AttemptKey(), Server: r.adapter.Server, Workspace: r.task.WorkspaceID, Agent: r.task.AgentID, Task: r.task.ID, Action: action}
}

func (r *running) refresh(ctx context.Context) error {
	status, err := r.adapter.Status.Status(ctx, r.task.ID)
	if err != nil || status != "running" {
		return fmt.Errorf("task status: %w", ErrDenied)
	}
	changed := false
	ref := identity.Ref{Server: r.adapter.Server, WorkspaceID: r.task.WorkspaceID, AgentID: r.task.AgentID}
	for name, connection := range r.connections {
		token := r.tokens[name]
		if time.Until(token.ExpiresAt) <= 10*time.Second {
			token, err = r.adapter.Issuer.AcquireForMCP(ctx, ref, connection.URL)
			if err != nil || time.Until(token.ExpiresAt) <= 10*time.Second {
				return fmt.Errorf("MCP issuance: %w", ErrDenied)
			}
			changed = true
		}
		grant := r.grant("renew")
		grant.URL = connection.URL
		grant.TokenHash = attempt.Fingerprint(token.Bearer())
		grant.ExpiresAt = min(token.ExpiresAt.Unix(), time.Now().Add(15*time.Second).Unix())
		// Authority registers the JWT fingerprint before it becomes visible to OpenCode.
		if err := r.adapter.Authority.Apply(ctx, grant); err != nil {
			return err
		}
		r.tokens[name] = token
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if changed && r.workload != nil {
		return r.project(ctx)
	}
	return nil
}

func (r *running) loop(ctx context.Context) {
	defer close(r.exited)
	command := r.adapter.Command
	if len(command) == 0 {
		command = []string{"/bin/sh", "-c", `exec opencode run --format json "$(cat /workspace/prompt.txt)"`}
	}
	agent := make(chan error, 1)
	go func() {
		agent <- r.workload.Stream(ctx, command, r.environment(), func(reader io.Reader) error { return r.events.read(ctx, reader) })
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var err error
	agentDone := false
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case err = <-agent:
			agentDone = true
			break loop
		case <-ticker.C:
			if r.events.idle(idleWatchdog) {
				err = &execution.IdleError{After: idleWatchdog}
				break loop
			}
			if err = r.refresh(ctx); err != nil {
				break loop
			}
		}
	}
	r.cancel()
	// Wait for the Docker exec client to exit before allowing container cleanup.
	if !agentDone {
		<-agent
	}
	r.done <- err
}

func (r *running) Result() execution.Result {
	if r.events == nil {
		return execution.Result{Disposable: true}
	}
	return execution.Result{Output: r.events.output.String(), SessionID: r.events.session, Disposable: true}
}

func (r *running) Wait(ctx context.Context) error {
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *running) Remove(ctx context.Context) error {
	r.once.Do(func() {
		// Relay access ends first, before the agent process is joined; revocation cancels in-flight streams.
		if r.relayToken != "" {
			r.adapter.Relay.Revoke(r.task.AttemptKey())
		}
		if r.inferenceToken != "" {
			r.adapter.InferenceRelay.Revoke(r.task.AttemptKey())
		}
		r.cancel()
		if r.started {
			<-r.exited
		}
		cleanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r.cleanupErr = r.adapter.Authority.Apply(cleanCtx, r.grant("revoke"))
		if r.workload != nil {
			r.cleanupErr = errors.Join(r.cleanupErr, r.workload.Remove(cleanCtx))
		}
		clear(r.tokens)
	})
	return r.cleanupErr
}

func (r *running) reject(cause error) error {
	if err := r.Remove(context.Background()); err != nil {
		return errors.Join(cause, err)
	}
	return &execution.RejectedError{Err: cause}
}
