package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// openCodeInference reads workspace gateway bindings and serves the inference relay
// (ADR 0012); keys stay in the controller: files, the external resolver or, with
// workspace_env, the workspace's Multica env read through api (ADR 0022).
func openCodeInference(ctx context.Context, c Config, api *multica.Client) (*inference.Service, *relay.Grants, error) {
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
	source.SetDebug(c.debugf())
	if config.WorkspaceEnv {
		source.UseWorkspaceEnv(func(ctx context.Context, workspace string) (inference.Env, bool, error) {
			return api.WorkspaceEnv(ctx, workspace)
		})
	}
	policy := relay.Policy{Allow: inference.RelayPath, Reserved: inference.ReservedHeaders, Withhold: true, HeaderTimeout: inference.HeaderTimeout}
	grants := relay.NewGrants(inference.RelayPrefix)
	err = serveRelay(ctx, "inference", grants, *c.OpenCode.InferenceRelay, policy, nil, nil)
	return source, grants, err
}
