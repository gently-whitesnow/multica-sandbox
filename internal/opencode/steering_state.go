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
	return syncOwnerDirectory(r.adapter.OwnerState)
}

// prepareNativeState runs before a server can resume orphaned executions at boot.
// A previous unclean owner is retired, never resumed under a new attempt's identity.
func (r *running) prepareNativeState(ctx context.Context) (bool, error) {
	if r.workDir == "" {
		return false, nil
	}
	if r.adapter.Steering == nil {
		return r.prepareACPState(ctx)
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
	return r.isolateNativeState(ctx, dir, quarantine, "http")
}

// A transport change deliberately retires HTTP state, including a clean owner.
// Removing its controller receipt after isolation lets later ACP resumes proceed;
// a future HTTP boot still treats its missing receipt as unknown, and retires ACP state.
func (r *running) prepareACPState(ctx context.Context) (bool, error) {
	if r.adapter.OwnerState == "" {
		return false, nil
	}
	if !filepath.IsAbs(r.adapter.OwnerState) {
		return false, fmt.Errorf("native HTTP owner state requires an absolute controller directory")
	}
	quarantine := "no"
	if _, err := os.Lstat(r.nativeOwnerPath()); err == nil {
		quarantine = "yes"
	} else if !os.IsNotExist(err) {
		return false, err
	}
	sum := sha256.Sum256([]byte(r.task.AttemptKey()))
	dir := "/workspace/work/.multica-sandbox/interrupted-" + hex.EncodeToString(sum[:8])
	lost, err := r.isolateNativeState(ctx, dir, quarantine, "acp")
	if err != nil || quarantine == "no" {
		return lost, err
	}
	if err := os.Remove(r.nativeOwnerPath()); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return lost, syncOwnerDirectory(r.adapter.OwnerState)
}

func syncOwnerDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (r *running) isolateNativeState(ctx context.Context, dir, quarantine, mode string) (bool, error) {
	out, err := r.workload.Capture(ctx, []string{"/bin/sh", "-c", nativeStateScript, SessionDB, dir, httpAttemptMarker, quarantine, mode}, nil)
	if err != nil {
		return false, fmt.Errorf("prepare isolated OpenCode HTTP state: %w", ErrDenied)
	}
	lost := strings.TrimSpace(string(out)) == "retired"
	if lost && r.resume != "" {
		r.retired, r.resume = r.resume, ""
	}
	return lost, nil
}

// Keep at most this attempt's quarantine. Retired databases are diagnostic copies,
// never recovery inputs; ordinary workdir files and native comments stay intact.
const nativeStateScript = `set -eu
changed=no
for old in "${0%/*}"/interrupted-*; do
 suffix=${old##*/interrupted-}
 case "$suffix" in ''|*[!0-9a-f]*) continue;; esac
 if [ "${#suffix}" = 16 ] && [ "$old" != "$1" ] && { [ -e "$old" ] || [ -L "$old" ]; }; then
  rm -rf "$old"; changed=yes
 fi
done
if [ -e "$2" ] || [ -L "$2" ] || { [ "$3" = yes ] && { [ -e "$0" ] || [ -L "$0" ] || [ -e "$0-wal" ] || [ -e "$0-shm" ]; }; }; then
 mkdir -p -m 700 "$1"
 for suffix in '' -wal -shm; do
  if [ -e "$0$suffix" ] || [ -L "$0$suffix" ]; then mv "$0$suffix" "$1/"; fi
 done
 printf retired; changed=yes
fi
if [ "$4" = http ]; then
 mkdir -p "${0%/*}"; printf active > "$2"; changed=yes
else
 rm -f "$2"
fi
if [ "$changed" = yes ]; then sync; fi`

func (r *running) cleanNativeState(ctx context.Context) error {
	if r.adapter.Steering == nil || r.workDir == "" || !r.nativeClean {
		return nil
	}
	if err := r.workload.Execute(ctx, []string{"/bin/sh", "-c", `set -eu; rm -f "$1"; sync`, "clean-http-owner", httpAttemptMarker}); err != nil {
		return err
	}
	return r.saveNativeOwner(true)
}
