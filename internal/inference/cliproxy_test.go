package inference

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCLIProxyCatalogUsesAvailableExactIDsAndEffectiveMetadata(t *testing.T) {
	const available = `{"data":[
{"id":"pool-a/response-demo","owned_by":"openai"},
{"id":"pool-b/response-demo","owned_by":"openai"},
{"id":"pool-c/message-demo","owned_by":"anthropic"},
{"id":"local-demo","owned_by":"local"},
{"id":"hidden-demo","owned_by":"openai"},
{"id":"unsupported-demo","owned_by":"openai"},
{"id":"pool-a/response-demo","owned_by":"anthropic"},
{"id":"bad\nname","owned_by":"openai"}
]}`
	const metadata = `{"models":[
{"slug":"pool-a/response-demo","display_name":"Response demo","context_window":64000,"max_context_window":1000000,"max_tokens":4096,"supported_in_api":true,"visibility":"list","default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"high","description":"private-provider-description"},{"effort":"low"}],"base_instructions":"private-provider-instructions"},
{"slug":"response-demo","context_window":80000,"max_tokens":8192,"supported_in_api":true},
{"slug":"pool-c/message-demo","context_window":200000,"max_tokens":32000,"supported_in_api":true,"supported_reasoning_levels":[{"effort":"xhigh"},{"effort":"high"}]},
{"slug":"hidden-demo","supported_in_api":true,"visibility":"hide"},
{"slug":"unsupported-demo","supported_in_api":false,"visibility":"list"},
{"slug":"not-available","supported_in_api":true,"context_window":400000,"max_tokens":128000}
]}`
	catalog, err := cliproxyModelsCatalog([]byte(available), []byte(metadata), "pool-a/response-demo")
	if err != nil || len(catalog.Models) != 4 || catalog.DefaultModel != "pool-a/response-demo" {
		t.Fatalf("catalog=%+v, err=%v", catalog, err)
	}
	response := catalog.Models["pool-a/response-demo"]
	if response.API != APIResponses || response.Label != "Response demo (pool-a/response-demo)" || response.Context != 64000 || response.Output != 4096 || response.Thinking == nil || response.Thinking.DefaultLevel != "high" {
		t.Fatalf("effective metadata changed: %+v", response)
	}
	if response.Thinking.SupportedLevels[0].Value != "low" || response.Thinking.SupportedLevels[1].Value != "high" {
		t.Fatalf("source reasoning levels lost: %+v", response.Thinking)
	}
	unknown := catalog.Models["pool-b/response-demo"]
	if unknown.API != APIResponses || unknown.Context != 0 || unknown.Output != 0 || unknown.Thinking != nil {
		t.Fatalf("metadata from another pool was substituted: %+v", unknown)
	}
	message := catalog.Models["pool-c/message-demo"]
	if message.API != APIMessages || message.Context != 200000 || message.Output != 32000 || message.Thinking == nil || len(message.Thinking.SupportedLevels) != 1 || message.Thinking.SupportedLevels[0].Value != "high" {
		t.Fatalf("Messages metadata changed: %+v", message)
	}
	data, _ := json.Marshal(catalog)
	if strings.Contains(string(data), "private-provider") || strings.Contains(string(data), "max_context_window") {
		t.Fatal("provider instructions or private metadata leaked")
	}
}

func TestCLIProxyCatalogRejectsInvalidMetadata(t *testing.T) {
	const available = `{"data":[{"id":"demo","owned_by":"openai"}]}`
	for name, metadata := range map[string]string{
		"malformed":      `{"models":`,
		"wrong schema":   `{"data":[]}`,
		"null models":    `{"models":null}`,
		"hidden only":    `{"models":[{"slug":"demo","supported_in_api":true,"visibility":"hide"}]}`,
		"unsupported":    `{"models":[{"slug":"demo","supported_in_api":false}]}`,
		"negative":       `{"models":[{"slug":"demo","supported_in_api":true,"context_window":-1}]}`,
		"oversize":       `{"models":[{"slug":"demo","supported_in_api":true,"context_window":10000001}]}`,
		"inconsistent":   `{"models":[{"slug":"demo","supported_in_api":true,"context_window":4096,"max_tokens":8192}]}`,
		"invalid number": `{"models":[{"slug":"demo","supported_in_api":true,"context_window":"64000"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cliproxyModelsCatalog([]byte(available), []byte(metadata), ""); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	for _, data := range []string{`{`, `{}`, `{"data":null}`, `{"data":[]}`, `{"data":[{"id":"bad\nname"}]}`} {
		if _, err := cliproxyModelsCatalog([]byte(data), []byte(`{"models":[]}`), ""); err == nil {
			t.Fatalf("invalid available models accepted: %s", data)
		}
	}
}

func TestCLIProxyCatalogPreservesPartialOrUnknownLimits(t *testing.T) {
	const available = `{"data":[{"id":"demo","owned_by":"openai"}]}`
	for _, tc := range []struct {
		metadata        string
		context, output int
	}{
		{`{"models":[]}`, 0, 0},
		{`{"models":[{"slug":"demo","supported_in_api":true,"context_window":64000}]}`, 64000, 0},
		{`{"models":[{"slug":"demo","supported_in_api":true,"max_tokens":4096}]}`, 0, 4096},
	} {
		catalog, err := cliproxyModelsCatalog([]byte(available), []byte(tc.metadata), "not-offered")
		if err != nil || catalog.Models["demo"].Context != tc.context || catalog.Models["demo"].Output != tc.output || catalog.DefaultModel != "" {
			t.Fatalf("partial metadata changed: %+v, %v", catalog, err)
		}
	}
}

func TestCLIProxyCatalogBoundsModelsDeterministically(t *testing.T) {
	models := []string{}
	for index := gatewayModels + 1; index >= 0; index-- {
		models = append(models, fmt.Sprintf(`{"id":"model-%04d","owned_by":"openai"}`, index))
	}
	available := `{"data":[` + strings.Join(models, ",") + `]}`
	catalog, err := cliproxyModelsCatalog([]byte(available), []byte(`{"models":[]}`), "")
	if err != nil || len(catalog.Models) != gatewayModels {
		t.Fatalf("catalog size=%d, err=%v", len(catalog.Models), err)
	}
	if _, exists := catalog.Models["model-0000"]; !exists {
		t.Fatal("lexically first model dropped")
	}
	if _, exists := catalog.Models[fmt.Sprintf("model-%04d", gatewayModels)]; exists {
		t.Fatal("model limit depends on upstream order")
	}
}
