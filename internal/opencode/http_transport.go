package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

var httpPollInterval = 250 * time.Millisecond
var supplementPollInterval = 5 * time.Second
var supplementIdleInterval = 30 * time.Second

// Leave room after withdrawal for finite transport teardown and worker return.
const httpWorkerGrace = 8 * time.Second

// converseHTTP owns one server for the whole attempt. Every request names its
// loopback origin; no API operation may discover or start a different service.
func (r *running) converseHTTP(ctx context.Context, _ io.Writer, stdout io.Reader) error {
	setup, cancelSetup := context.WithTimeout(ctx, acpSetup)
	defer cancelSetup()
	remaining, err := nativeHTTPReady(setup, stdout)
	if err != nil {
		return err
	}
	go drainHTTPOutput(ctx, remaining)
	r.v2 = true
	session, err := r.httpSession(setup)
	if err != nil {
		return err
	}
	if len(r.connections) > 0 {
		if err := r.httpMCP(setup); err != nil {
			return err
		}
		select {
		case <-time.After(mcpSettle):
		case <-setup.Done():
			return setup.Err()
		}
	}
	r.events.begin(session)
	stopReports := r.events.flushEvery(reportInterval)
	defer stopReports()
	since := time.Now()
	r.spent.session, r.spent.since = acp.SessionId(session), since
	defer r.httpSpent(ctx, session, since)
	streamCtx, stopStream := context.WithCancel(ctx)
	h := newHTTPTurn(r, session)
	streamDone := make(chan error, 1)
	go func() {
		defer close(streamDone)
		streamDone <- r.httpEvents(streamCtx, func(event httpEvent) {
			h.consume(event)
			h.unattended(streamCtx, event)
		})
	}()
	defer func() {
		stopStream()
		select {
		case <-streamDone:
		case <-time.After(cancelGrace):
		}
	}()
	select {
	case <-h.connected:
	case <-setup.Done():
		return setup.Err()
	case err := <-streamDone:
		return httpStreamFailure(err)
	}
	body := map[string]any{"id": "msg_" + rand.Text(), "text": string(r.prompt), "delivery": "steer", "metadata": r.httpInputMetadata("initial")}
	if err := r.httpAPI(setup, "POST", "/api/session/"+session+"/prompt", body, nil); err != nil {
		return err
	}
	return h.run(ctx, streamDone)
}

func (r *running) httpEvents(ctx context.Context, consume func(httpEvent)) error {
	command := []string{"/bin/sh", "-c", `exec wget -T 0 -q -O - --header "Authorization: Basic $SANDBOX_HTTP_AUTH" http://127.0.0.1:4096/api/event`}
	return r.stream(ctx, command, &r.stderr, func(_ io.Writer, stdout io.Reader) error {
		if closer, ok := stdout.(io.Closer); ok {
			stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
			defer stop()
		}
		err := readHTTPEvents(ctx, stdout, consume)
		if err != nil && ctx.Err() == nil {
			_, _ = r.stderr.Write([]byte("native event stream: " + err.Error() + "\n"))
		}
		return err
	})
}

func (h *httpTurn) run(ctx context.Context, streamDone <-chan error) error {
	return h.runWorker(ctx, streamDone, h.supplements)
}

func (h *httpTurn) runWorker(ctx context.Context, streamDone <-chan error, worker func(context.Context)) error {
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); worker(workerCtx) }()
	joined := false
	defer func() {
		stopWorker()
		if joined {
			return
		}
		select {
		case <-workerDone:
		case <-time.After(httpWorkerGrace):
		}
	}()
	ticker := time.NewTicker(httpPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stopWorker()
			grace, cancel := context.WithTimeout(context.Background(), cancelGrace)
			_ = h.r.httpAPI(grace, "POST", "/api/session/"+h.session+"/interrupt?resume=false", nil, nil)
			cancel()
			return ctx.Err()
		case err := <-streamDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return httpStreamFailure(err)
		case <-ticker.C:
		case <-h.changed:
		}
		started, busy, terminal, _, _, shells, failure := h.status()
		h.mu.Lock()
		fatal := h.fatal
		h.mu.Unlock()
		if fatal {
			return failure
		}
		if !started || busy || !terminal || shells != 0 {
			continue
		}
		// Join the input worker while the native owner is alive, so an admission
		// losing completion can withdraw its undelivered inbox item.
		stopWorker()
		select {
		case <-workerDone:
			joined = true
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(httpWorkerGrace):
			joined = true
			return &execution.HostFailure{Message: "Sandbox could not settle OpenCode additional input before cleanup"}
		}
		if h.r.adapter.Steering != nil {
			replay, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelGrace)
			_, _ = h.r.adapter.Steering.ReplaySupplementAcks(replay)
			cancel()
		}
		return h.finish(ctx, failure)
	}
}

func nativeHTTPReady(ctx context.Context, stdout io.Reader) (io.Reader, error) {
	reader := bufio.NewReaderSize(stdout, 64<<10)
	var ready struct {
		URL string `json:"url"`
	}
	done := make(chan error, 1)
	go func() {
		line, err := reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			done <- err
			return
		}
		done <- json.Unmarshal(line, &ready)
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-done:
		if err != nil || ready.URL != nativeHTTPOrigin {
			return nil, &execution.AgentFailure{Message: "OpenCode HTTP owner did not report the expected loopback endpoint"}
		}
		return reader, nil
	}
}

func httpStreamFailure(err error) error {
	if err == nil || errors.Is(err, streamCut) {
		return streamCut
	}
	return &execution.AgentFailure{Message: "OpenCode native HTTP event stream failed"}
}

func (h *httpTurn) finish(ctx context.Context, failure error) error {
	var info struct {
		Data nativeSession `json:"data"`
	}
	if err := h.r.httpAPI(ctx, "GET", "/api/session/"+h.session, nil, &info); err != nil {
		return err
	}
	if info.Data.Time.Busy > info.Data.Time.Idle {
		return &execution.AgentFailure{Message: "OpenCode started another execution during completion"}
	}
	if failure != nil {
		return failure
	}
	if info.Data.Outcome != "succeeded" {
		return &execution.AgentFailure{Message: "OpenCode HTTP execution did not succeed"}
	}
	h.markClean(ctx)
	output, _, _ := h.r.events.result()
	h.mu.Lock()
	usage := h.r.spent.usage
	h.mu.Unlock()
	if output == "" && h.r.events.tools.Load() == 0 && usage == (multica.Usage{}) {
		return &execution.AgentFailure{Message: "OpenCode turn ended without output or usage"}
	}
	return nil
}

func (h *httpTurn) supplements(ctx context.Context) {
	if h.r.adapter.Steering == nil || !h.r.adapter.Steering.SupplementsNegotiated(h.r.task) {
		return
	}
	updates := h.r.adapter.Steering.SupplementUpdates(h.r.task)
	for {
		started, busy, _, _, _, _, _ := h.status()
		if started && busy {
			progress, err := h.r.adapter.Steering.ProcessSupplement(ctx, h.r.task, h.session, h.deliver)
			if err == nil && progress {
				continue
			}
		}
		timer := time.NewTimer(supplementIdleInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-updates:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (r *running) httpInputMetadata(kind string) map[string]string {
	return map[string]string{"multica_attempt": r.task.AttemptKey(), "sandbox_input": kind}
}
