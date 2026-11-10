package multica

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type startResponseBody struct{ reads, closes int }

func (b *startResponseBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("response stream failed")
}

func (b *startResponseBody) Close() error { b.closes++; return nil }

type startRoundTrip func(*http.Request) (*http.Response, error)

func (f startRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStartWithoutSteeringDoesNotReadResponse(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f, c, _ := newSupplementFixture(t)
		if !enabled {
			c.supplements = nil
		}
		task := f.task
		if enabled {
			task.IssueID = ""
		}
		body := &startResponseBody{}
		requests := 0
		c.http.Transport = startRoundTrip(func(r *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
		})
		if err := c.Start(context.Background(), task); err != nil || body.reads != 0 || body.closes != 1 || requests != 1 {
			t.Fatalf("ordinary Start depended on response-body transport: reads=%d closes=%d requests=%d err=%v", body.reads, body.closes, requests, err)
		}
	}
}

func TestSteeringStartRequiresReadableNegotiation(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	body := &startResponseBody{}
	c.http.Transport = startRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
	})
	original := retrySleep
	retrySleep = func(context.Context, time.Duration) error { return nil }
	defer func() { retrySleep = original }()
	if err := c.Start(context.Background(), f.task); err == nil || body.reads == 0 || c.SupplementsNegotiated(f.task) {
		t.Fatal("unreadable steering negotiation advertised support", err)
	}
}

func TestResponseBodyReadFailureClassificationIsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f, c, _ := newSupplementFixture(t)
		if !enabled {
			c.supplements = nil
		}
		body := &startResponseBody{}
		c.http.Transport = startRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: body, Header: http.Header{}}, nil
		})
		_, err := c.Status(context.Background(), f.task.ID)
		if err == nil || errors.Is(err, errTransport) != enabled || Transient(err) != enabled || body.reads != 1 || body.closes != 1 {
			t.Fatalf("body read classification enabled=%v reads=%d closes=%d err=%v", enabled, body.reads, body.closes, err)
		}
		if !enabled && err.Error() != "read Multica response" {
			t.Fatal("default body read error changed", err)
		}
	}
}

func TestSteeringStartRetriesLostBodyThenNegotiates(t *testing.T) {
	f, c, _ := newSupplementFixture(t)
	body, requests := &startResponseBody{}, 0
	c.http.Transport = startRoundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		var response io.ReadCloser = body
		if requests > 1 {
			response = io.NopCloser(strings.NewReader(`{"supplement_capability":"task-supplement-v1"}`))
		}
		return &http.Response{StatusCode: 200, Body: response, Header: http.Header{}}, nil
	})
	original := retrySleep
	retrySleep = func(context.Context, time.Duration) error { return nil }
	defer func() { retrySleep = original }()
	if err := c.Start(context.Background(), f.task); err != nil || !c.SupplementsNegotiated(f.task) || requests != 2 || body.closes != 1 {
		t.Fatalf("lost steering negotiation body did not retry safely: requests=%d err=%v", requests, err)
	}
}
