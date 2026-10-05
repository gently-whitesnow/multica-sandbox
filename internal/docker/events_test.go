package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

func TestNativeFailureDoesNotExposeResponseBody(t *testing.T) {
	for _, status := range []string{"403", "429", "999"} {
		data := []byte(`{"type":"error","error":{"name":"APIError","data":{"statusCode":` + status + `,"message":"private-sentinel","responseBody":"private-sentinel","responseHeaders":{"Authorization":"Bearer private-sentinel"}}}}`)
		err := agentFailure(data)
		var failure *execution.AgentFailure
		if !errors.As(err, &failure) || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("unsafe failure", err)
		}
		if status != "999" && failure.Status == 0 {
			t.Fatal("HTTP status lost")
		}
		if status == "999" && failure.Status != 0 {
			t.Fatal("invalid HTTP status accepted")
		}
	}
	if agentFailure([]byte(`{"type":"text","part":{"text":"error"}}`)) != nil {
		t.Fatal("text classified as failure")
	}
}
