package opencode

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// This stream intentionally detaches cancellation like the Docker owner's
// conversation transport. Finite requests must close their own blocking pipes.
type stalledHTTPWorkload struct{ nativeHTTPWorkload }

func (w *stalledHTTPWorkload) StreamStderr(ctx context.Context, _ []string, _ map[string]string, _ io.Writer, converse func(io.Writer, io.Reader) error) error {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	defer inReader.Close()
	defer outWriter.Close()
	err := converse(inWriter, outReader)
	if ctx.Err() == nil {
		return errors.New("stalled request ended before deadline")
	}
	return err
}

func TestHTTPAPIRequestDeadlineClosesDetachedPipes(t *testing.T) {
	for _, body := range []any{nil, map[string]string{"text": strings.Repeat("guidance", 1024)}} {
		r, _, _ := nativeHTTPFixture()
		r.workload = &stalledHTTPWorkload{}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		started := time.Now()
		err := r.httpAPI(ctx, "POST", "/api/session/ses_root/prompt", body, nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
			t.Fatalf("deadline=%v elapsed=%s", err, time.Since(started))
		}
	}
}

func TestHTTPTextProofUsesConstantMemoryAndPreservesSuffix(t *testing.T) {
	r, h, _ := nativeHTTPFixture()
	part := strings.Repeat("a", 64<<10)
	for range 32 {
		h.consume(httpEvent{Type: "session.text.delta", Data: nativeEvent(t, "", "session.text.delta", "ses_root", map[string]any{"assistantMessageID": "msg_answer", "delta": part}).Data})
	}
	key := "ses_root:msg_answer:0:text"
	proof := h.texts[key]
	if proof == nil || proof.size != 2<<20 || proof.digest.Size() != sha256.Size {
		t.Fatal("text retained more than its fixed digest and byte count")
	}
	complete := strings.Repeat(part, 32) + " end"
	h.consume(nativeEvent(t, "ended", "session.text.ended", "ses_root", map[string]any{"assistantMessageID": "msg_answer", "text": complete}))
	output, _, _ := r.events.result()
	if output != complete || len(h.texts) != 0 {
		t.Fatal("ended text lost its suffix or retained its proof")
	}
}

func TestHTTPCompletionWaitsForAmbiguousWithdrawalWithoutHostFailure(t *testing.T) {
	r, h, w := nativeHTTPFixture()
	r.events.chunk("text", "original turn completed")
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if args[4] == "POST" {
			h.consume(nativeEvent(t, "terminal", "session.execution.succeeded", "ses_root", nil))
		}
		if args[4] == "GET" {
			return []byte(`{"data":{"id":"ses_root","outcome":"succeeded","time":{"busy":1000,"idle":2000}}}`), nil
		}
		return nil, nil // DELETE204 deliberately supplies no cancellation proof.
	}
	workerError := make(chan error, 1)
	worker := func(ctx context.Context) { workerError <- h.deliver(ctx, "msg_pending", "generic guidance") }
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	started := time.Now()
	err := h.runWorker(ctx, make(chan error), worker)
	if err != nil || time.Since(started) > 4*time.Second {
		t.Fatalf("completion: err=%v elapsed=%s", err, time.Since(started))
	}
	if err := <-workerError; !errors.Is(err, errHTTPInputEnded) || !h.unsafeResume || r.nativeClean {
		t.Fatalf("uncertain input: err=%v unsafe=%v clean=%v", err, h.unsafeResume, r.nativeClean)
	}
}
