package docker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type limitedOutput struct{ bytes.Buffer }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 1024*1024 {
		return 0, fmt.Errorf("Docker response exceeds limit")
	}
	return w.Buffer.Write(p)
}
func command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out limitedOutput
	cmd.Stdout = &out
	// Diagnostics may contain image metadata. Do not expose them as task output.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s failed: %w", args[0], err)
	}
	return out.Bytes(), nil
}
