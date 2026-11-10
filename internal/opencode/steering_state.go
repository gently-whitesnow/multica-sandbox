package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const httpAttemptMarker = "/workspace/work/.multica-sandbox/http-attempt-active"

type nativeOwner struct {
	Attempt string `json:"attempt"`
	Clean   bool   `json:"clean"`
}

func (o nativeOwner) valid(t multica.Task) bool {
	parts := strings.SplitN(o.Attempt, ":", 4)
	if len(parts) != 4 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, parts[3])
	return err == nil && parts[0] == t.WorkspaceID && parts[1] == t.RuntimeID && multica.ValidID(parts[2])
}

func (r *running) nativeOwnerPath() string {
	sum := sha256.Sum256([]byte(r.adapter.Server + "\x00" + r.task.WorkspaceID + "\x00" + r.task.AgentID + "\x00" + r.workDir))
	return filepath.Join(r.adapter.OwnerState, hex.EncodeToString(sum[:])+".json")
}

func (r *running) saveNativeOwner(clean bool) error {
	if !filepath.IsAbs(r.adapter.OwnerState) {
		return fmt.Errorf("native HTTP owner state requires an absolute controller directory")
	}
	if err := os.MkdirAll(r.adapter.OwnerState, 0700); err != nil {
		return err
	}
	data, _ := json.Marshal(nativeOwner{r.task.AttemptKey(), clean})
	file, err := os.CreateTemp(r.adapter.OwnerState, ".owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), r.nativeOwnerPath()); err != nil {
		return err
	}
	dir, err := os.Open(r.adapter.OwnerState)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// prepareNativeState runs before a server can resume orphaned executions at boot.
// A previous unclean owner is retired, never resumed under a new attempt's identity.
func (r *running) prepareNativeState(ctx context.Context) (bool, error) {
	if r.adapter.Steering == nil || r.workDir == "" {
		return false, nil
	}
	sum := sha256.Sum256([]byte(r.task.AttemptKey()))
	dir := "/workspace/work/.multica-sandbox/interrupted-" + hex.EncodeToString(sum[:8])
	var previous nativeOwner
	data, readErr := os.ReadFile(r.nativeOwnerPath())
	trustedClean := readErr == nil && json.Unmarshal(data, &previous) == nil && previous.valid(r.task) && previous.Clean
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, readErr
	}
	if err := r.saveNativeOwner(false); err != nil {
		return false, err
	}
	quarantine := "yes"
	if trustedClean {
		quarantine = "no"
	}
	script := `set -eu
mkdir -p "${0%/*}"
if [ -e "$2" ] || [ -L "$2" ] || { [ "$3" = yes ] && { [ -e "$0" ] || [ -L "$0" ] || [ -e "$0-wal" ] || [ -e "$0-shm" ]; }; }; then
 mkdir -p -m 700 "$1"
 for suffix in '' -wal -shm; do
  if [ -e "$0$suffix" ] || [ -L "$0$suffix" ]; then mv "$0$suffix" "$1/"; fi
 done
 printf retired
fi
printf active > "$2"
sync`
	out, err := r.workload.Capture(ctx, []string{"/bin/sh", "-c", script, SessionDB, dir, httpAttemptMarker, quarantine}, nil)
	if err != nil {
		return false, fmt.Errorf("prepare isolated OpenCode HTTP state: %w", ErrDenied)
	}
	lost := strings.TrimSpace(string(out)) == "retired"
	if lost && r.resume != "" {
		r.retired, r.resume = r.resume, ""
	}
	return lost, nil
}

func (r *running) cleanNativeState(ctx context.Context) error {
	if r.adapter.Steering == nil || r.workDir == "" || !r.nativeClean {
		return nil
	}
	if err := r.workload.Execute(ctx, []string{"/bin/sh", "-c", `set -eu; rm -f "$1"; sync`, "clean-http-owner", httpAttemptMarker}); err != nil {
		return err
	}
	return r.saveNativeOwner(true)
}
