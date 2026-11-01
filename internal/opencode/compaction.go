package opencode

import (
	"fmt"
	"slices"
	"strings"
)

// CheckCompactionImage rejects a configured cap on releases whose native buffer
// semantics are not verified. A zero cap preserves the existing image contract.
func CheckCompactionImage(output []byte, maxTokens int) error {
	if maxTokens < 0 {
		return fmt.Errorf("compaction_max_tokens must be nonnegative")
	}
	if maxTokens == 0 {
		return nil
	}
	version := imageVersion(output)
	if !strings.HasPrefix(version, "2.") || !slices.Contains(Supported, version) {
		return fmt.Errorf("compaction_max_tokens requires verified OpenCode 2.x; set 0 for native policy (image: %.32q)", version)
	}
	return nil
}

// projectCompaction uses OpenCode 2.2.2's native buffer setting, preserving model
// metadata and a smaller model's native safety margin. The native reserve formula
// is versioned behavior from packages/core/src/session/compaction.ts.
func projectCompaction(config map[string]any, window, maxTokens int) {
	if window <= 0 || maxTokens <= 0 {
		return
	}
	reserve := window * 15 / 100
	if window >= 32000 {
		reserve = max(reserve, 16000)
	}
	if window-reserve <= maxTokens {
		return
	}
	config["compaction"] = map[string]any{"auto": true, "buffer": window - maxTokens}
}
