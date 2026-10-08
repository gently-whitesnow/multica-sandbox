package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// openCodeInference reads workspace gateway bindings and serves the inference relay
// (ADR 0012); keys stay in controller-only files or the external resolver.
func openCodeInference(ctx context.Context, c Config) (*inference.Service, *relay.Grants, error) {
	if (c.OpenCode.InferenceFile == "") != (c.OpenCode.InferenceRelay == nil) {
		return nil, nil, fmt.Errorf("inference_file and inference_relay must be configured together")
	}
	if c.OpenCode.InferenceFile == "" {
		return nil, nil, nil
	}
	config, err := inference.ReadConfig(c.OpenCode.InferenceFile)
	if err != nil {
		return nil, nil, err
	}
	source, err := inference.New(config, strings.TrimRight(c.Server, "/"))
	if err != nil {
		return nil, nil, err
	}
	policy := relay.Policy{Allow: inference.RelayPath, Reserved: inference.ReservedHeaders, Withhold: true, HeaderTimeout: inference.HeaderTimeout}
	grants := relay.NewGrants(inference.RelayPrefix)
	err = serveRelay(ctx, "inference", grants, *c.OpenCode.InferenceRelay, policy, nil)
	return source, grants, err
}
