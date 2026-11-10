package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type httpStreamWorkload struct {
	nativeHTTPWorkload
	start  chan struct{}
	events []httpEvent
}

func (w *httpStreamWorkload) StreamStderr(ctx context.Context, args []string, env map[string]string, _ io.Writer, converse func(io.Writer, io.Reader) error) error {
	return w.Stream(ctx, args, env, converse)
}

func (w *httpStreamWorkload) Stream(ctx context.Context, args []string, env map[string]string, converse func(io.Writer, io.Reader) error) error {
	if args[0] == "opencode" || args[0] == "timeout" || len(args) > 3 && args[3] == "native-http" {
		return w.nativeHTTPWorkload.Stream(ctx, args, env, converse)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		defer writer.Close()
		_, _ = io.WriteString(writer, "data: {\"type\":\"server.connected\",\"data\":{}}\n\n")
		select {
		case <-ctx.Done():
			return
		case <-w.start:
		}
		for _, event := range w.events {
			raw, _ := json.Marshal(event)
			if _, err := io.WriteString(writer, "data: "+string(raw)+"\n\n"); err != nil {
				return
			}
		}
		<-ctx.Done()
	}()
	return converse(&bytes.Buffer{}, reader)
}

func TestHTTPTransportOwnsServiceThroughTerminalAndReportsUsage(t *testing.T) {
	r, _, _ := nativeHTTPFixture()
	w := &httpStreamWorkload{start: make(chan struct{})}
	r.workload = w
	w.events = []httpEvent{
		nativeEvent(t, "1", "session.execution.started", "ses_root", nil),
		nativeEvent(t, "2", "session.step.started", "ses_root", nil),
		nativeEvent(t, "3", "session.text.delta", "ses_root", map[string]any{"assistantMessageID": "msg_answer", "delta": "completed"}),
		nativeEvent(t, "4", "session.text.ended", "ses_root", map[string]any{"assistantMessageID": "msg_answer", "text": "completed"}),
		nativeEvent(t, "5", "session.step.ended", "ses_root", map[string]any{"tokens": map[string]int{"input": 11, "output": 7}}),
		nativeEvent(t, "6", "session.execution.succeeded", "ses_root", nil),
	}
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		switch {
		case args[4] == "POST" && strings.HasSuffix(args[5], "/prompt"):
			close(w.start)
			return []byte(`{}`), nil
		case strings.Contains(args[5], "/message?"):
			return json.Marshal(map[string]any{"data": []map[string]any{{"id": "msg_answer", "type": "assistant", "time": map[string]int64{"created": time.Now().UnixMilli()}, "tokens": map[string]int{"input": 11, "output": 7}}}})
		default:
			return []byte(`{"data":{"id":"ses_root","outcome":"succeeded","location":{"directory":"/workspace/work"},"time":{"busy":1000,"idle":2000}}}`), nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.converseHTTP(ctx, &bytes.Buffer{}, strings.NewReader(`{"url":"http://127.0.0.1:4096"}`)); err != nil {
		t.Fatal(err)
	}
	output, session, usage := r.events.result()
	if output != "completed" || session != "ses_root" || usage != (multica.Usage{Input: 11, Output: 7}) {
		t.Fatalf("output=%s session=%s usage=%+v", output, session, usage)
	}
	for _, args := range w.requests {
		if len(args) < 6 || args[2] != nativeHTTPOrigin {
			t.Fatalf("request did not target the owned service: %v", args)
		}
	}
}

func TestHTTPTransportRefusesForeignOwnerReadiness(t *testing.T) {
	r, _, w := nativeHTTPFixture()
	err := r.converseHTTP(context.Background(), &bytes.Buffer{}, strings.NewReader(`{"url":"http://127.0.0.1:4097"}`))
	if err == nil || len(w.requests) != 0 {
		t.Fatalf("foreign owner used: err=%v requests=%v", err, w.requests)
	}
}
