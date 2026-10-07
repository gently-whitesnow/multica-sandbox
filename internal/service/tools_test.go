package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolDeclarations(t *testing.T) {
	image := "example.invalid/tool@sha256:" + strings.Repeat("a", 64)
	o := &OpenCodeConfig{MulticaCLI: "example.invalid/cli@sha256:" + strings.Repeat("b", 64)}
	got, err := bundles(o, []Tool{{Name: "jq", Image: image}, {Name: "go", Image: image, Path: []string{"bin", "pkg/tool"}, Check: []string{"bin/go", "version"}}})
	if err != nil || len(got) != 3 || got[0].Target != "/opt/multica-sandbox/multica" || got[2].Target != "/opt/multica-sandbox/tools/go" ||
		strings.Join(got[2].Dirs(), ":") != "/opt/multica-sandbox/tools/go/bin:/opt/multica-sandbox/tools/go/pkg/tool" {
		t.Fatalf("got %+v %v", got, err)
	}
	for name, tools := range map[string][]Tool{
		"name":      {{Name: "../x", Image: image}},
		"upper":     {{Name: "JQ", Image: image}},
		"duplicate": {{Name: "jq", Image: image}, {Name: "jq", Image: image}},
		"absolute":  {{Name: "jq", Image: image, Check: []string{"/bin/sh", "-c", "true"}}},
		"escape":    {{Name: "jq", Image: image, Check: []string{"../../bin/sh"}}},
		"too many":  make([]Tool, 17),
	} {
		if _, err := bundles(&OpenCodeConfig{}, tools); err == nil {
			t.Errorf("%s: invalid tool declaration accepted", name)
		}
	}
}

func TestToolsRequireAgentAdapter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.json")
	config := `{"server":"https://multica.example.com","daemon":"10000000-0000-4000-8000-000000000002","image":"x","command":["/bin/true"],"timeout":"1s","tools":[{"name":"jq","image":"x"}]}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil || !strings.Contains(err.Error(), "opencode adapter") {
		t.Fatalf("tools without an agent adapter: %v", err)
	}
}
