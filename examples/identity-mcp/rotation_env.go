package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Inspect the real native processes, not only the planned execution projection.
func rotationEnvironment(ctx context.Context, owner string) error {
	data, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+owner).Output()
	containers := strings.Fields(string(data))
	if err != nil || len(containers) != 2 {
		return fmt.Errorf("expected two native identity attempts")
	}
	command := `environment=$(cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n')
    printf '%s\n' "$environment" | grep -qx 'USER_SETTING=native-env-delivered' || exit 1
    if printf '%s\n' "$environment" | grep -qE 'IAM_CLIENT_ID=|IAM_CLIENT_SECRET=|eyJ[A-Za-z0-9_-]+\.'; then exit 2; fi
    if grep -raqE 'IAM_CLIENT_SECRET|eyJ[A-Za-z0-9_-]+\.' /workspace/opencode.json; then exit 3; fi`
	for _, container := range containers {
		// Start returns before the agent process is exec'd, so wait for its environment;
		// a projected IAM value or JWT (exit 2, 3) fails at once.
		deadline := time.Now().Add(30 * time.Second)
		for {
			err := exec.CommandContext(ctx, "docker", "exec", "--user", "65532:65532", container, "sh", "-c", command).Run()
			var exit *exec.ExitError
			if err == nil {
				break
			}
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || time.Now().After(deadline) {
				return fmt.Errorf("native identity ENV delivery/nonprojection failed: %w", err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	return nil
}
