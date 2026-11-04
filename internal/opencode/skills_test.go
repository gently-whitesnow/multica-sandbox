package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func TestSkillTreeMirrorsUpstreamLayout(t *testing.T) {
	files, _, skipped, err := skillTree([]multica.Skill{
		{Name: "PR review", Description: "Reviews: pull requests", Content: "Check the diff.", Files: []multica.SkillFile{{Path: "references/a.md", Content: "a"}, {Path: "./SKILL.md", Content: "duplicate"}}},
		{Name: "pr-review", Content: "---\nname: other\nlicense: MIT\n---\nBody"},
		{Name: "  ", Content: "---\ndescription: d\n---\nx"},
	})
	if err != nil || len(skipped) != 0 {
		t.Fatal(err, skipped)
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

func TestSkillTreeRejectsEscapes(t *testing.T) {
	for name, path := range map[string]string{"parent": "../b/x", "nested": "r/../../x", "absolute": "/etc/x", "backslash": `r\x`, "empty": "", "dot": "r/.."} {
		// An escaping file denies the claim even in a skill that would be skipped anyway.
		skills := []multica.Skill{{Name: "ok"}, {Name: "a", Files: make([]multica.SkillFile, maxSkillFiles+1)}}
		skills[1].Files[0] = multica.SkillFile{Path: path, Content: "x"}
		for i := 1; i <= maxSkillFiles; i++ {
			skills[1].Files[i] = multica.SkillFile{Path: fmt.Sprintf("f%d", i)}
		}
		if _, _, _, err := skillTree(skills); !errors.Is(err, ErrDenied) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestSkillTreeSkipsWhatDoesNotFit(t *testing.T) {
	big := strings.Repeat("x", maxSkillFile)
	many := make([]multica.SkillFile, maxSkillFiles+1)
	for i := range many {
		many[i] = multica.SkillFile{Path: fmt.Sprintf("f%d", i)}
	}
	skills := []multica.Skill{
		{Name: "twice", Files: []multica.SkillFile{{Path: "r/x", Content: "1"}, {Path: "r/./x", Content: "2"}}},
		{Name: "file dir", Files: []multica.SkillFile{{Path: "r", Content: "1"}, {Path: "r/x", Content: "2"}}},
		{Name: "dir file", Files: []multica.SkillFile{{Path: "r/x/y", Content: "1"}, {Path: "r", Content: "2"}}},
		{Name: "dir skill", Files: []multica.SkillFile{{Path: "SKILL.md/x", Content: "1"}}},
		{Name: "large", Content: big + "x"},
		{Name: "files", Files: many},
		{Name: "keep"},
	}
	for i := range 9 {
		skills = append(skills, multica.Skill{Name: fmt.Sprintf("big%d", i), Content: big[:maxSkillFile-4096]})
	}
	skills = append(skills, multica.Skill{Name: "Keep"}, multica.Skill{Name: "small", Content: "fits after the total"})
	for range maxSkills {
		skills = append(skills, multica.Skill{Name: "x"})
	}
	files, kept, skipped, err := skillTree(skills)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range skipped {
		names = append(names, s.Name+": "+s.Reason)
	}
	got := strings.Join(names, "\n")
	for _, want := range []string{"twice: a file is listed twice", "file-dir: a file path is also a directory", "dir-file: a file path is also a directory", "dir-skill: a file path is also a directory",
		"large: a file exceeds 1 MiB", "files: more than 256 files", "big8: over the 8 MiB total"} {
		if !strings.Contains(got, want) {
			t.Errorf("skipped lacks %q: %s", want, got)
		}
	}
	// Claim order decides: eight big skills fit the total, later small ones still do, the
	// limit cuts the tail, and slugs follow the installed set.
	if len(kept) != maxSkills || kept[0].Name != "keep" || kept[9].Name != "Keep" || kept[10].Name != "small" || len(kept)+len(skipped) != len(skills) ||
		files["keep-multica/SKILL.md"] == nil || files["big7/SKILL.md"] == nil || files["big8/SKILL.md"] != nil || files["twice/SKILL.md"] != nil || !strings.Contains(got, "x: over the 64-skill limit") {
		t.Fatalf("kept %d, skipped %d: %s", len(kept), len(skipped), got)
	}
	again, _, _, _ := skillTree(skills)
	if !reflect.DeepEqual(files, again) {
		t.Fatal("skill selection is not deterministic")
	}
	total := 0
	for _, data := range files {
		total += len(data)
	}
	if total > maxSkillsTotal {
		t.Fatalf("installed %d bytes", total)
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
