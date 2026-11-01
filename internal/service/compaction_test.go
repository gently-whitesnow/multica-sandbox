package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadConfigValidatesCompactionCap(t *testing.T) {
	for _, tc := range []struct {
		cap       int
		inference string
		allowed   bool
	}{
		{400000, "/config/inference.json", true},
		{0, "", true},
		{-1, "/config/inference.json", false},
		{400000, "", false},
	} {
		path := filepath.Join(t.TempDir(), "controller.json")
		body := fmt.Sprintf(`{"daemon":"10000000-0000-4000-8000-000000000001","image":"fixture","timeout":"1m","opencode":{"compaction_max_tokens":%d,"inference_file":%q}}`, tc.cap, tc.inference)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		config, err := ReadConfig(path)
		if (err == nil) != tc.allowed {
			t.Fatalf("cap=%d inference=%q: %v", tc.cap, tc.inference, err)
		}
		if err == nil && config.OpenCode.CompactionMaxTokens != tc.cap {
			t.Fatal("configured compaction cap was lost")
		}
	}
}
