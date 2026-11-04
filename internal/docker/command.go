package docker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
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
	return commandWithin(ctx, commandTimeout, args...)
}

// commandTimeout bounds ordinary engine requests; tests shorten it.
var commandTimeout = 30 * time.Second

// RemoveTimeout bounds attempt removal apart from the caller's cleanup deadline: killed
// attempt processes throttled at memory.high and swapped out exit slowly (ADR 0020), and
// a failed cleanup restarts the controller and every running task with it.
const RemoveTimeout = 3 * time.Minute

func removal(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), RemoveTimeout)
}

// commandWithin runs the docker CLI; tests replace it.
var commandWithin = runDocker

func runDocker(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out limitedOutput
	var stderr cappedOutput
	cmd.Stdout, cmd.Stderr = &out, &stderr
	// Diagnostics may contain image metadata. Do not expose them as task output. Engine
	// messages name the cause; attempt and image processes write their own stderr.
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" && args[0] != "exec" && !slices.Contains(args, "--attach") {
			return nil, fmt.Errorf("docker %s failed: %w: %.256q", args[0], err, detail)
		}
		return nil, fmt.Errorf("docker %s failed: %w", args[0], err)
	}
	return out.Bytes(), nil
}
