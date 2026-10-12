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
	return commandWithin(ctx, 30*time.Second, args...)
}

// RemoveTimeout bounds attempt removal apart from the caller's cleanup deadline: killed
// attempt processes throttled at memory.high and swapped out exit slowly (ADR 0020), and
// a failed cleanup restarts the controller and every running task with it.
const RemoveTimeout = 3 * time.Minute

func removal(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), RemoveTimeout)
}

func commandWithin(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
