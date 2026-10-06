package opencode

import (
	"context"
	"errors"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

type catalogInference struct {
	changingInference
	catalog      inference.Catalog
	err          error
	scope        inference.Scope
	catalogCalls int
}

func (s *catalogInference) Catalog(_ context.Context, scope inference.Scope) (inference.Catalog, error) {
	s.scope = scope
	s.catalogCalls++
	return s.catalog, s.err
}
func TestModelSelectionIsIntentNotCatalogAuthorization(t *testing.T) {
	for _, mode := range []string{"default", "explicit", "stale", "outage", "no-reasoning", "new-effort", "no-default", "bad-model", "empty-qualified-model", "bad-effort"} {
		t.Run(mode, func(t *testing.T) {
			source := &catalogInference{catalog: inference.Catalog{DefaultModel: "demo", Models: map[string]inference.Model{"demo": {Context: 64000, Output: 4096, Thinking: &inference.Thinking{DefaultLevel: "medium", SupportedLevels: []inference.ThinkingLevel{{Value: "medium", Label: "Medium"}}}}}}}
			task := safeTask()
			wantModel, wantEffort := "demo", "medium"
			switch mode {
			case "explicit":
				task.Agent.Model = "managed-inference/demo"
				task.Agent.ThinkingLevel = "high"
				wantEffort = "high"
			case "stale":
				task.Agent.Model = "new-model"
				wantModel = "new-model"
				wantEffort = ""
			case "outage":
				task.Agent.Model = "new-model"
				task.Agent.ThinkingLevel = "high"
				source.catalog = inference.Catalog{}
				source.err = errors.New("unavailable")
				wantModel, wantEffort = "new-model", "high"
			case "no-reasoning":
				source.catalog.Models["demo"] = inference.Model{Context: 64000, Output: 4096}
				wantEffort = ""
			case "new-effort":
				task.Agent.ThinkingLevel = "deep"
				wantEffort = "deep"
			case "no-default":
				source.catalog.DefaultModel = ""
			case "empty-qualified-model":
				task.Agent.Model = "managed-inference/"
			case "bad-model":
				task.Agent.Model = "bad\nmodel"
			case "bad-effort":
				task.Agent.ThinkingLevel = "high\n"
			}
			r := &running{adapter: &Adapter{Server: "https://controller.example.invalid", Inference: source}, task: task}
			got, err := r.selectInference(context.Background())
			denied := mode == "no-default" || mode == "bad-model" || mode == "empty-qualified-model" || mode == "bad-effort"
			if denied {
				if err == nil {
					t.Fatal("unmappable selection accepted")
				}
				return
			}
			if err != nil || got.model != wantModel || got.effort != wantEffort {
				t.Fatalf("selection = %+v, %v", got, err)
			}
			if source.scope != (inference.Scope{Server: r.adapter.Server, WorkspaceID: task.WorkspaceID}) {
				t.Fatal("catalog selectors did not come from trusted controller/claim")
			}

		})
	}
}
func TestCatalogChangesDoNotStopIdentityRotation(t *testing.T) {
	source := &catalogInference{}
	r := &running{adapter: &Adapter{Inference: source, Authority: &stubAuthority{}}, connections: map[string]Remote{}}
	if _, err := r.refreshInference(context.Background(), identity.Ref{}); err != nil {
		t.Fatal(err)
	}
	source.catalog = inference.Catalog{DefaultModel: "different"}
	r.inference.Token.ExpiresAt = r.inference.Token.ExpiresAt.Add(-2e9)
	if _, err := r.refreshInference(context.Background(), identity.Ref{}); err != nil || source.calls != 2 {
		t.Fatal("catalog change interfered with renewal", err)
	}
	if source.catalogCalls != 0 {
		t.Fatal("renewal queried catalog")
	}
}
