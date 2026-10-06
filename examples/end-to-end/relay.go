package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const relayLocal = "http://127.0.0.1:8092"

// startRelay serves the workspace-key inference relay (ADR 0012) on the sandbox network
// alias inference-relay. The LiteLLM workspace key stays in this trusted process.
func startRelay(ctx context.Context) (*relay.Grants, error) {
	grants := relay.NewGrants(inference.RelayPrefix)
	handler, err := relay.New(grants, relay.Policy{Allow: inference.RelayPath, Limit: 32 << 20, Reserved: inference.ReservedHeaders, Withhold: true, HeaderTimeout: inference.HeaderTimeout})
	if err != nil {
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", ":8092")
	if err != nil {
		return nil, fmt.Errorf("inference relay listener unavailable")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	context.AfterFunc(ctx, func() { _ = server.Close() })
	return grants, nil
}

// grantInference binds the fixture task to the workspace key with claim attribution.
func grantInference(grants *relay.Grants, attempt string) (string, error) {
	key, err := os.ReadFile("/config/workspace-key")
	if err != nil {
		return "", fmt.Errorf("workspace key unavailable; run setup")
	}
	return grants.Issue(attempt, relay.Upstream{Origin: "http://litellm:4000", Credential: strings.TrimSpace(string(key)), Header: inference.Attribution(workspace, agentID, taskID)})
}
