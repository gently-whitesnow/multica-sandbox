package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func TestSkillTreeMirrorsUpstreamLayout(t *testing.T) {
	files, err := skillTree([]multica.Skill{
		{Name: "PR review", Description: "Reviews: pull requests", Content: "Check the diff.", Files: []multica.SkillFile{{Path: "references/a.md", Content: "a"}, {Path: "./SKILL.md", Content: "duplicate"}}},
		{Name: "pr-review", Content: "---\nname: other\nlicense: MIT\n---\nBody"},
		{Name: "  ", Content: "---\ndescription: d\n---\nx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"pr-review/SKILL.md":         "---\nname: pr-review\ndescription: \"Reviews: pull requests\"\n---\n\nCheck the diff.",
		"pr-review/references/a.md":  "a",
		"pr-review-multica/SKILL.md": "---\nname: pr-review-multica\nlicense: MIT\n---\nBody",
		"skill/SKILL.md":             "---\nname: skill\ndescription: d\n---\nx",
	}
	if len(files) != len(want) {
		t.Fatalf("files %v", files)
	}
	for name, content := range want {
		if string(files[name]) != content {
			t.Errorf("%s = %q, want %q", name, files[name], content)
		}
	}
}

func TestSkillTreeRejectsEscapesAndExcess(t *testing.T) {
	big := strings.Repeat("x", maxSkillFile+1)
	for name, skills := range map[string][]multica.Skill{
		"parent":    {{Name: "a", Files: []multica.SkillFile{{Path: "../b/x", Content: "x"}}}},
		"nested":    {{Name: "a", Files: []multica.SkillFile{{Path: "r/../../x", Content: "x"}}}},
		"absolute":  {{Name: "a", Files: []multica.SkillFile{{Path: "/etc/x", Content: "x"}}}},
		"backslash": {{Name: "a", Files: []multica.SkillFile{{Path: `r\x`, Content: "x"}}}},
		"empty":     {{Name: "a", Files: []multica.SkillFile{{Path: "", Content: "x"}}}},
		"dot":       {{Name: "a", Files: []multica.SkillFile{{Path: "r/..", Content: "x"}}}},
		"twice":     {{Name: "a", Files: []multica.SkillFile{{Path: "r/x", Content: "1"}, {Path: "r/./x", Content: "2"}}}},
		"file dir":  {{Name: "a", Files: []multica.SkillFile{{Path: "r", Content: "1"}, {Path: "r/x", Content: "2"}}}},
		"dir file":  {{Name: "a", Files: []multica.SkillFile{{Path: "r/x/y", Content: "1"}, {Path: "r", Content: "2"}}}},
		"dir skill": {{Name: "a", Files: []multica.SkillFile{{Path: "SKILL.md/x", Content: "1"}}}},
		"large":     {{Name: "a", Content: big}},
		"count":     make([]multica.Skill, maxSkills+1),
		"total":     {{Name: "a", Content: big[:maxSkillFile]}, {Name: "b", Content: big[:maxSkillFile]}, {Name: "c", Content: big[:maxSkillFile]}, {Name: "d", Content: big[:maxSkillFile]}, {Name: "e", Content: big[:maxSkillFile]}, {Name: "f", Content: big[:maxSkillFile]}, {Name: "g", Content: big[:maxSkillFile]}, {Name: "h", Content: big[:maxSkillFile]}},
	} {
		if _, err := skillTree(skills); !errors.Is(err, ErrDenied) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestEnsureSkillFrontmatterKeepsUpstreamRewrites(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{"---\r\nname: Old\r\nlicense: MIT\r\n---\r\nBody", "---\r\nname: s\r\nlicense: MIT\r\n---\r\nBody"},
		{"---\nname: \"upstream\ncontinued\"\nother: 1\n---\n", "---\nname: s\nother: 1\n---\n"},
		{"---\nname: x\n# keep\n\nother: y\n---\n", "---\nname: s\n# keep\n\nother: y\n---\n"},
		{"---\ndescription: only\n---\nb", "---\nname: s\ndescription: only\n---\nb"},
		{"---\nname: a: b\n---\nbody", "---\nname: s\ndescription: \"d\"\n---\n\nbody"},
		{"---\nname: &n \"true\"\ndisable-model-invocation: *n\n---\nx", "---\nname: s\ndisable-model-invocation: \"true\"\n---\nx"},
	} {
		if got := ensureSkillFrontmatter(tc.content, "s", "d"); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.content, got, tc.want)
		}
	}
}

func TestBriefListsVisibleSkills(t *testing.T) {
	task := relayTask(t)
	task.Agent.Skills = []multica.Skill{{Name: "Hidden", Content: "---\ndisable-model-invocation: true\n---\n"}, {Name: "hidden"}, {Name: "multica-platform"}}
	_, brief, err := Prompt(task, true, false, Fresh)
	if err != nil {
		t.Fatal(err)
	}
	text := string(brief)
	if !strings.Contains(text, "## Skills\n\nYou have the following skills installed (discovered automatically):\n\n- **hidden-multica**\n- **multica-platform**\n\nFor a Multica platform action") ||
		strings.Contains(text, "- **hidden**\n") || !strings.Contains(text, "## Sub-issue Creation\n\n`--status todo` starts") ||
		!strings.Contains(text, "read `references/issues.md` in the `multica-platform` skill") || strings.Index(text, "## Skills") > strings.Index(text, "## Important: Always Use") {
		t.Fatalf("brief skills differ from upstream: %s", text)
	}
	// v0.5.2 still points at the pre-merge skill when only it is installed.
	task.Agent.Skills = []multica.Skill{{Name: "multica-working-on-issues"}}
	if _, brief, _ = Prompt(task, true, false, Fresh); !strings.Contains(string(brief), "stages. Before creating sub-issues, read the `multica-working-on-issues` skill — it covers") || strings.Contains(string(brief), "For a Multica platform action") {
		t.Fatalf("pre-merge skill pointer: %s", brief)
	}
	task.Agent.Skills = nil
	if _, brief, _ = Prompt(task, true, false, Fresh); strings.Contains(string(brief), "## Skills") || strings.Contains(string(brief), "Before creating sub-issues") {
		t.Fatal("skill sections without skills")
	}
}

func TestChatPromptNamesSelectedSkills(t *testing.T) {
	task := chatClaim()
	task.Agent.Skills = []multica.Skill{{ID: "s1", Name: "PR review"}, {ID: "s2", Name: "Deploy"}}
	task.ChatMessage = `Use [/Deploy](slash://skill/s2) then [/PR \[x\]](slash://skill/s1), [/again](slash://skill/s2) and [/gone](slash://skill/s9).`
	prompt, _, err := Prompt(task, true, false, Fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "Audience: direct room.\n\nExplicitly selected skills:\n- Deploy\n- PR review\n\nUser message:\n") {
		t.Fatalf("chat prompt: %q", prompt)
	}
}

func TestAdapterProjectsSkillsForUpstreamTasks(t *testing.T) {
	task := relayTask(t)
	task.Agent.Skills = []multica.Skill{{Name: "Review", Description: "Reviews", Content: "Body", Files: []multica.SkillFile{{Path: "ref.md", Content: "r"}}}}
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1)}
	a := Adapter{Server: "https://multica.example.invalid", Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Relay: relay.NewGrants(multica.RelayPrefix), RelayURL: "http://multica-relay:8091"}
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	<-workload.envs
	defer run.Remove(context.Background())
	var config struct{ Permission map[string]string }
	if workload.trees != 1 || string(workload.files[SkillsDir+"/review/ref.md"]) != "r" || !strings.HasPrefix(string(workload.files[SkillsDir+"/review/SKILL.md"]), "---\nname: review\n") ||
		json.Unmarshal(workload.files["/workspace/opencode.json"], &config) != nil || config.Permission["*"] != "allow" || config.Permission["skill"] == "deny" {
		t.Fatalf("skills not projected: %d %v %v", workload.trees, workload.files, config.Permission)
	}
	task.Agent.Skills[0].Files[0].Path = "../escape"
	if _, err := a.Start(context.Background(), task); !errors.As(err, new(*execution.RejectedError)) {
		t.Fatalf("escaping skill accepted: %v", err)
	}
}
