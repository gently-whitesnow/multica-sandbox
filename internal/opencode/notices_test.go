package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func TestBriefNamesWhatTheAttemptLeftOut(t *testing.T) {
	task := relayTask(t)
	task.Agent.MCPConfig = json.RawMessage(`{"mcpServers":{"selected":{"url":"https://tools.example.invalid/mcp","headers":{"Authorization":"Bearer {iam:access_token}"}},"local":{"command":"private-command","env":{"TOKEN":"private-mcp-env"}}}}`)
	task.Agent.Skills = []multica.Skill{{Name: "Review", Content: "Body"}, {Name: "Huge", Content: strings.Repeat("x", maxSkillFile+1)}}
	if err := json.Unmarshal([]byte(`{"FEED_URL":"https://feed.example.invalid","GH_TOKEN":"private-gh","GIT_AUTHOR_NAME":"private-author","http_proxy":"private-proxy","IAM_CLIENT_ID":"private-client"}`), &task.Agent.CustomEnv); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1)}
	a := Adapter{Server: "https://multica.example.invalid", Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Relay: relay.NewGrants(multica.RelayPrefix), RelayURL: "http://multica-relay:8091", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	env := <-workload.envs
	defer run.Remove(context.Background())
	brief, prompt := string(workload.files[BriefPath]), string(run.(*running).prompt)
	for _, want := range []string{"## Sandbox Notices\n", "- MCP server `local` not connected: stdio MCP is not supported", "- Skill `huge` not installed: a file exceeds 1 MiB",
		"- Environment variables not passed (reserved for the sandbox): `GH_TOKEN`, `GIT_AUTHOR_NAME`, `http_proxy`."} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(prompt, "Sandbox Notices") || strings.Contains(brief, "IAM_CLIENT_ID") || strings.Contains(brief, "FEED_URL") || !strings.Contains(brief, "- **review**") || strings.Contains(brief, "- **huge**") ||
		workload.files[SkillsDir+"/review/SKILL.md"] == nil || workload.files[SkillsDir+"/huge/SKILL.md"] != nil || env["GH_TOKEN"] != "" || env["FEED_URL"] == "" {
		t.Fatalf("notices or projection differ: %v\n%s", env, brief)
	}
	for _, want := range []string{"MCP servers not connected", "local: stdio MCP", "skills not installed", "huge: a file exceeds", "agent environment variables not passed", "GH_TOKEN"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q: %s", want, logs.String())
		}
	}
	if strings.Contains(brief+logs.String(), "private-") || strings.Contains(brief+logs.String(), "example.invalid") {
		t.Fatalf("notice leaked a value: %s\n%s", logs.String(), brief)
	}
}

func TestBriefOmitsNoticesWhenNothingIsLeftOut(t *testing.T) {
	var logs bytes.Buffer
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1)}
	a := Adapter{Server: "https://multica.example.invalid", Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Relay: relay.NewGrants(multica.RelayPrefix), RelayURL: "http://multica-relay:8091", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	run, err := a.Start(context.Background(), relayTask(t))
	if err != nil {
		t.Fatal(err)
	}
	<-workload.envs
	defer run.Remove(context.Background())
	if strings.Contains(string(workload.files[BriefPath])+string(run.(*running).prompt), "Sandbox Notices") || logs.Len() != 0 {
		t.Fatalf("notices without anything left out: %s", logs.String())
	}
}

func TestMCPWithoutRelaySkipsAnonymousServers(t *testing.T) {
	task := safeTask()
	task.Agent.MCPConfig = json.RawMessage(`{"mcp":{"docs":{"type":"remote","url":"https://docs.example.invalid/mcp","headers":{"X-Api-Key":"private-key"}}}}`)
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1)}
	a := Adapter{Issuer: &stubIssuer{}, Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}}
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	<-workload.envs
	defer run.Remove(context.Background())
	r := run.(*running)
	if len(r.connections) != 0 || r.mcpToken != "" || !strings.HasSuffix(string(r.prompt), "## Sandbox Notices\n\nThis sandbox left the following out of your configuration. Do not try to restore it; if the task depends on it, say so in your result.\n\n- MCP server `docs` not connected: this sandbox has no MCP relay.") ||
		strings.Contains(string(r.prompt), "private-") {
		t.Fatalf("MCP without relay: %v %q", r.connections, r.prompt)
	}
}

func TestNoticeSectionBoundsLists(t *testing.T) {
	var skipped []Skipped
	var env []string
	for i := range maxNoticeItems + 4 {
		skipped = append(skipped, Skipped{fmt.Sprintf("s%02d", i), "reason"})
		env = append(env, fmt.Sprintf("GIT_%02d", i))
	}
	section := Notices{MCP: skipped, Skills: skipped[:1], Env: env, GitRelay: true}.Section()
	if strings.Count(section, "- MCP server `") != maxNoticeItems || !strings.Contains(section, "- 4 more MCP servers not connected.\n- Skill `s00` not installed: reason.\n") ||
		!strings.Contains(section, "`GIT_15` and 4 more. Git and forge access (`git`, `gh`, `glab`) goes through the sandbox relay") || strings.Contains(section, "GIT_16") {
		t.Fatalf("section: %s", section)
	}
	if prompt, brief := (Notices{}).Apply([]byte("p"), []byte("b")); string(prompt) != "p" || string(brief) != "b" {
		t.Fatal("empty notices changed the prompt or brief")
	}
}
