package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

type nativeHTTPWorkload struct {
	stubWorkload
	mu          sync.Mutex
	requests    [][]string
	commands    [][]string
	captureHTTP func(context.Context, []string) ([]byte, error)
}

func (w *nativeHTTPWorkload) Stream(ctx context.Context, args []string, env map[string]string, converse func(io.Writer, io.Reader) error) error {
	w.mu.Lock()
	w.commands = append(w.commands, append([]string{}, args...))
	w.mu.Unlock()
	reader, writer := io.Pipe()
	input := &httpTestInput{closed: make(chan struct{})}
	normalized := append([]string{}, args...)
	post := len(args) > 3 && args[3] == "native-http"
	if !post {
		close(input.closed)
	}
	go func() {
		defer writer.Close()
		select {
		case <-ctx.Done():
			_ = writer.CloseWithError(ctx.Err())
			return
		case <-input.closed:
		}
		if post {
			normalized = []string{"opencode", "--server", nativeHTTPOrigin, "api", "POST", strings.TrimPrefix(args[4], nativeHTTPOrigin), "--data", input.String()}
		}
		data, err := w.Capture(ctx, normalized, env)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_, _ = writer.Write(data)
	}()
	defer reader.Close()
	return converse(input, reader)
}

func (w *nativeHTTPWorkload) StreamStderr(ctx context.Context, args []string, env map[string]string, _ io.Writer, converse func(io.Writer, io.Reader) error) error {
	return w.Stream(ctx, args, env, converse)
}

type httpTestInput struct {
	bytes.Buffer
	closed chan struct{}
}

func (i *httpTestInput) Close() error { close(i.closed); return nil }

func (w *nativeHTTPWorkload) Capture(ctx context.Context, args []string, env map[string]string) ([]byte, error) {
	w.mu.Lock()
	w.requests = append(w.requests, append([]string{}, args...))
	w.mu.Unlock()
	if w.captureHTTP != nil {
		return w.captureHTTP(ctx, args)
	}
	return []byte(`{}`), nil
}

func nativeHTTPFixture() (*running, *httpTurn, *nativeHTTPWorkload) {
	w := &nativeHTTPWorkload{}
	r := &running{adapter: &Adapter{}, events: &eventStream{}, workload: w, task: safeTask(), nativePassword: "opaque-owner-password"}
	h := newHTTPTurn(r, "ses_root")
	h.started, h.busy, h.execution = true, true, 1
	return r, h, w
}

func TestHTTPDeliveryRequiresPromotionAndNewStep(t *testing.T) {
	_, h, w := nativeHTTPFixture()
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		parts := strings.Split(args[5], "/")
		return json.Marshal(map[string]any{"data": map[string]any{"id": parts[len(parts)-1], "type": "user", "time": map[string]int{"created": 1000}}})
	}
	if h.consumed(context.Background(), "msg_guidance", 1) {
		t.Fatal("canonical history alone acknowledged delivery")
	}
	h.consume(nativeEvent(t, "1", "session.inbox.delivered", "ses_root", map[string]any{"inboxID": "msg_guidance"}))
	if h.consumed(context.Background(), "msg_guidance", 1) {
		t.Fatal("promotion alone acknowledged delivery")
	}
	h.consume(nativeEvent(t, "2", "session.step.started", "ses_root", nil))
	if !h.consumed(context.Background(), "msg_guidance", 1) {
		t.Fatal("same-execution processing boundary was not accepted")
	}
	h.consume(nativeEvent(t, "3", "session.execution.started", "ses_root", nil))
	if !h.consumed(context.Background(), "msg_guidance", 1) {
		t.Fatal("later execution erased immutable processing proof")
	}
	h.consume(nativeEvent(t, "4", "session.inbox.delivered", "ses_root", map[string]any{"inboxID": "msg_late"}))
	h.consume(nativeEvent(t, "5", "session.execution.started", "ses_root", nil))
	h.consume(nativeEvent(t, "6", "session.step.started", "ses_root", nil))
	if h.consumed(context.Background(), "msg_late", 2) {
		t.Fatal("another execution supplied a missing processing proof")
	}
}

func TestHTTPDeliveryCompletionRaceWithdrawsWithoutInterrupt(t *testing.T) {
	_, h, w := nativeHTTPFixture()
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if args[4] == "POST" {
			h.consume(nativeEvent(t, "terminal", "session.execution.succeeded", "ses_root", nil))
		}
		return []byte(`{}`), nil
	}
	err := h.deliver(context.Background(), "msg_guidance", "generic guidance")
	if !errors.Is(err, errHTTPInputEnded) {
		t.Fatalf("delivery=%v", err)
	}
	withdrawn := false
	for _, args := range w.requests {
		if strings.Contains(strings.Join(args, " "), "interrupt") {
			t.Fatal("steering interrupted the original engine")
		}
		if args[4] == "DELETE" && args[5] == "/api/session/ses_root/inbox/msg_guidance" {
			withdrawn = true
		}
	}
	if !withdrawn {
		t.Fatal("completion race left pending guidance for session resume")
	}
	if !h.unsafeResume {
		t.Fatal("ambiguous promotion could be reused by the next attempt")
	}
}

func TestHTTPDeliveryLostAdmissionUsesStableID(t *testing.T) {
	_, h, w := nativeHTTPFixture()
	requests, admissions := 0, map[string]bool{}
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if args[4] == "POST" {
			var body struct {
				ID       string
				Resume   bool
				Delivery string
			}
			if err := json.Unmarshal([]byte(args[7]), &body); err != nil {
				t.Fatal(err)
			}
			if body.Resume || body.Delivery != "steer" {
				t.Fatal("input could start another execution")
			}
			admissions[body.ID] = true
			requests++
			if requests == 1 {
				h.consume(nativeEvent(t, "delivered", "session.inbox.delivered", "ses_root", map[string]any{"inboxID": body.ID}))
				h.consume(nativeEvent(t, "step", "session.step.started", "ses_root", nil))
				return nil, errors.New("lost response")
			}
		}
		return []byte(`{"data":{"id":"msg_guidance","type":"user","time":{"created":1000}}}`), nil
	}
	if err := h.deliver(context.Background(), "msg_guidance", "generic guidance"); err != nil {
		t.Fatal(err)
	}
	h.busy = false
	if err := h.deliver(context.Background(), "msg_guidance", "generic guidance"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(admissions) != 1 {
		t.Fatalf("requests=%d admissions=%d", requests, len(admissions))
	}
}

func TestHTTPUnattendedRejectsOnlyOwnedSessions(t *testing.T) {
	r, h, w := nativeHTTPFixture()
	permission := nativeEvent(t, "1", "permission.asked", "ses_foreign", map[string]any{"id": "per_fixture"})
	h.unattended(context.Background(), permission)
	if len(w.requests) != 0 {
		t.Fatal("foreign permission was rejected")
	}
	permission.Data.SessionID = "ses_root"
	h.unattended(context.Background(), permission)
	if len(w.requests) != 1 || w.requests[0][5] != "/api/session/ses_root/permission/per_fixture/reply" {
		t.Fatalf("requests=%v", w.requests)
	}
	for _, arg := range w.requests[0] {
		if strings.Contains(arg, r.nativePassword) {
			t.Fatal("owner credential in API argv")
		}
	}
}

func TestHTTPResumedSessionRequiresOwnedWorkdir(t *testing.T) {
	r, _, w := nativeHTTPFixture()
	r.resume = "ses_prior"
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if args[4] == "POST" {
			return []byte(`{"data":{"id":"ses_fresh","location":{"directory":"/workspace/work"}}}`), nil
		}
		return []byte(`{"data":{"id":"ses_prior","location":{"directory":"/foreign"}}}`), nil
	}
	id, err := r.httpSession(context.Background())
	if err != nil || id != "ses_fresh" || r.retired != "ses_prior" || !strings.Contains(string(r.prompt), "Session Continuity Notice") {
		t.Fatalf("session=%s retired=%s err=%v", id, r.retired, err)
	}
}
