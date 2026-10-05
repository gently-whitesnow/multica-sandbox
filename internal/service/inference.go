package service

import (
	"strings"

	agentidentity "github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func openCodeInference(c Config) (opencode.Inference, error) {
	if c.OpenCode.InferenceFile == "" {
		return nil, nil
	}
	config, err := inference.ReadConfig(c.OpenCode.InferenceFile)
	if err != nil {
		return nil, inference.ErrDenied
	}
	credentials, err := agentidentity.ReadConfig(config.IdentityFile)
	if err != nil {
		return nil, inference.ErrDenied
	}
	issuer, err := agentidentity.New(credentials, strings.TrimRight(c.Server, "/"))
	if err != nil {
		return nil, err
	}
	return inference.New(config, strings.TrimRight(c.Server, "/"), issuer)
}
