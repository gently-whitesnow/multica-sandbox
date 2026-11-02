package opencode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

func TestCompactionCapPreservesNativeModelLimits(t *testing.T) {
	for _, tc := range []struct {
		name                string
		window, cap, buffer int
	}{
		{"large", 1000000, 400000, 600000},
		{"medium", 500000, 400000, 100000},
		{"small-native-margin", 272000, 400000, 0},
		{"native-is-earlier", 450000, 400000, 0},
		{"native-threshold-equals-cap", 1000000, 850000, 0},
		{"disabled", 1000000, 0, 0},
		{"unknown", 0, 400000, 0},
		{"small-window", 20000, 16000, 4000},
		{"small-window-native", 20000, 17000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := 128000
			if tc.window > 0 {
				output = min(output, tc.window)
			}
			source := &catalogInference{catalog: inference.Catalog{DefaultModel: "demo", Models: map[string]inference.Model{"demo": {Context: tc.window, Output: output}}}}
			r := &running{adapter: &Adapter{Inference: source, CompactionMaxTokens: tc.cap}, task: safeTask()}
			data, err := r.configureInference(context.Background(), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Compaction *struct {
					Auto   bool
					Buffer int
				}
				Provider map[string]struct {
					Models map[string]struct{ Limit map[string]int }
				}
			}
			if err = json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if tc.buffer == 0 {
				if config.Compaction != nil {
					t.Fatalf("native compaction overridden: %s", data)
				}
			} else if config.Compaction == nil || !config.Compaction.Auto || config.Compaction.Buffer != tc.buffer {
				t.Fatalf("wrong compaction buffer: %s", data)
			}
			limits := config.Provider[inference.ChatProvider].Models["demo"].Limit
			if tc.window > 0 && limits["context"] != tc.window {
				t.Fatalf("context metadata changed: %s", data)
			}
			if limits["output"] != output {
				t.Fatalf("output metadata changed: %s", data)
			}
		})
	}
}

func TestCompactionCapRequiresVerifiedNativeBufferSemantics(t *testing.T) {
	for _, tc := range []struct {
		report  string
		cap     int
		allowed bool
	}{
		{"version opencode v2.2.2\n", 400000, true},
		{"version 2.2.2\n", 400000, true},
		{"version opencode v2.2.5\n", 400000, true},
		{"version 1.18.35\n", 400000, false},
		{"version opencode v2.99.0\n", 400000, false},
		{"version opencode v2.2.2\n", -1, false},
		{"", 400000, false},
		{"version 1.18.35\n", 0, true},
	} {
		if err := CheckCompactionImage([]byte(tc.report), tc.cap); (err == nil) != tc.allowed {
			t.Fatalf("report=%q cap=%d: %v", tc.report, tc.cap, err)
		}
	}
}
