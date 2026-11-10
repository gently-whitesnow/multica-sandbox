package opencode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHTTPMCPRequiresSelectedDiscovery(t *testing.T) {
	r, _, w := nativeHTTPFixture()
	r.connections = map[string]Remote{"fixture": {}}
	calls := 0
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if args[5] != "/api/mcp?directory=%2Fworkspace%2Fwork" {
			t.Fatal(args)
		}
		calls++
		if calls == 1 {
			return []byte(`{"data":[{"name":"foreign","status":{"status":"connected"}},{"name":"fixture","status":{"status":"pending"}}]}`), nil
		}
		return []byte(`{"data":[{"name":"fixture","status":{"status":"connected"}}]}`), nil
	}
	if err := r.httpMCP(context.Background()); err != nil || calls != 2 {
		t.Fatalf("discovery=%v calls=%d", err, calls)
	}
}

func TestHTTPMCPDoesNotHideFailureOrExposeError(t *testing.T) {
	r, _, w := nativeHTTPFixture()
	r.connections = map[string]Remote{"fixture": {}}
	w.captureHTTP = func(context.Context, []string) ([]byte, error) {
		return []byte(`{"data":[{"name":"fixture","status":{"status":"failed","error":"private endpoint and credential"}}]}`), nil
	}
	err := r.httpMCP(context.Background())
	if err == nil || !strings.Contains(err.Error(), "MCP server is failed") || strings.Contains(err.Error(), "private") {
		t.Fatalf("discovery=%v", err)
	}
}

func TestHTTPMCPMissingDiscoveryIsContextBounded(t *testing.T) {
	r, _, w := nativeHTTPFixture()
	r.connections = map[string]Remote{"fixture": {}}
	w.captureHTTP = func(context.Context, []string) ([]byte, error) { return []byte(`{"data":[]}`), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.httpMCP(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("discovery=%v", err)
	}
}
