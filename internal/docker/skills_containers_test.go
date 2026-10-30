//go:build containers

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

// TestOfficialImagesDiscoverProjectedSkills is the skills conformance check (ADR 0018):
// every verified OpenCode image finds a tree projected into the global skill root.
func TestOfficialImagesDiscoverProjectedSkills(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	owner := fmt.Sprintf("93000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	template, peer := "sandbox-skills-"+owner, "sandbox-skills-peer-"+owner
	t.Cleanup(func() {
		_, _ = command(context.Background(), "rm", "-fv", peer)
		_, _ = command(context.Background(), "network", "rm", template)
	})
	if _, err := command(ctx, "network", "create", "--internal", template); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "run", "-d", "--name", peer, "--network", template, testImage, "sleep", "600"); err != nil {
		t.Fatal(err)
	}
	tree := map[string][]byte{
		"fixture-skill/SKILL.md":           []byte("---\nname: fixture-skill\ndescription: \"Fixture: use when checking skill discovery.\"\n---\n\nBody.\n"),
		"fixture-skill/references/note.md": []byte("note\n"),
	}
	for i, image := range opencode.Images {
		t.Run(opencode.Supported[i], func(t *testing.T) {
			if _, err := command(ctx, "image", "inspect", image); err != nil {
				t.Skip("pull the images.txt images first")
			}
			backend := &Projected{Backend: Backend{Image: image, Owner: owner, Command: []string{"/bin/sh"}}, Network: template, Peers: []string{peer}}
			t.Cleanup(func() { _ = backend.Reconcile(context.Background()) })
			run, err := backend.Start(ctx, "skills:"+opencode.Supported[i], execution.Workdir{})
			if err != nil {
				t.Fatal(err)
			}
			setup := `mkdir -p /workspace/config/opencode; printf "{}" > /workspace/config/opencode/opencode.json; printf "*\n" > /workspace/config/opencode/.gitignore`
			if err := run.Execute(ctx, []string{"/bin/sh", "-c", setup}); err != nil {
				t.Fatal(err)
			}
			if err := run.WriteTree(ctx, opencode.SkillsDir, tree); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(opencode.Supported[i], "2.") {
				// 2.x lists no skills on the CLI; it loads them from its config roots, and the
				// ACP spike observed the projected skill in the system prompt.
				out, err := run.Capture(ctx, []string{"opencode", "debug", "config"}, nil)
				if err != nil || !strings.Contains(string(out), `"path": "/workspace/config/opencode"`) {
					t.Fatalf("projected config root not loaded: %v %.512s", err, out)
				}
				return
			}
			out, err := run.Capture(ctx, []string{"opencode", "debug", "skill"}, nil)
			var skills []struct{ Name, Description, Location string }
			if err != nil || json.Unmarshal(out, &skills) != nil {
				t.Fatalf("skill listing: %v %.512s", err, out)
			}
			for _, s := range skills {
				if s.Name == "fixture-skill" && s.Location == opencode.SkillsDir+"/fixture-skill/SKILL.md" && s.Description != "" {
					return
				}
			}
			t.Fatalf("projected skill not discovered: %+v", skills)
		})
	}
}
