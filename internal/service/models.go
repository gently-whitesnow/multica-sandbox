package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	agentidentity "github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

type agentFleetAPI struct {
	*multica.Client
	inference opencode.Inference
	scopes    sync.Map
	server    string
}

func (a *agentFleetAPI) Register(ctx context.Context, ws, daemon string) (multica.Runtime, error) {
	rt, err := a.RegisterProvider(ctx, ws, daemon, "opencode")
	if err == nil {
		a.scopes.Store(rt.ID, ws)
	}
	return rt, err
}
func (a *agentFleetAPI) Heartbeat(ctx context.Context, rt string) error {
	if a.inference == nil {
		return a.Client.Heartbeat(ctx, rt)
	}
	return a.HeartbeatModels(ctx, rt, func(ctx context.Context, agent string) ([]multica.ModelEntry, error) {
		workspace, ok := a.scopes.Load(rt)
		if !ok {
			return nil, fmt.Errorf("runtime scope unavailable")
		}
		catalog, err := a.inference.Catalog(ctx, agentidentity.Ref{Server: a.server, WorkspaceID: workspace.(string), AgentID: agent})
		if err != nil {
			return nil, err
		}
		models := []multica.ModelEntry{}
		for name, m := range catalog.Models {
			label := m.Label
			if label == "" {
				label = name
			}
			entry := multica.ModelEntry{ID: "managed-inference/" + name, Provider: "managed-inference", Label: label, Default: name == catalog.DefaultModel, Context: m.Context, Output: m.Output}
			if m.Thinking != nil {
				entry.Thinking = &multica.ModelThinking{DefaultLevel: m.Thinking.DefaultLevel}
				for _, level := range m.Thinking.SupportedLevels {
					entry.Thinking.SupportedLevels = append(entry.Thinking.SupportedLevels, multica.ThinkingLevel{Value: level.Value, Label: level.Label})
				}
			}
			models = append(models, entry)
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		return models, nil
	})
}

func (a *agentFleetAPI) Fail(ctx context.Context, id string, cause error, result execution.Result) error {
	var failure *execution.AgentFailure
	if errors.As(cause, &failure) {
		return a.AgentFail(ctx, id, failure.Status, result)
	}
	return a.AgentFail(ctx, id, 0, result)
}
