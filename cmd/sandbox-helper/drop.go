package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// mirrorName is a reference mirror directory (ADR 0035): 32 hex digits of its key's SHA-256.
var mirrorName = regexp.MustCompile(`^[0-9a-f]{32}$`)

// mirrorUsage is the output of drop: the volume's usage and the bytes of each mirror left.
type mirrorUsage struct {
	usage
	Mirrors map[string]int64 `json:"mirrors"`
}

// drop removes the named mirrors of a mirror volume with their unfinished rebuilds and
// measures what remains. It removes nothing unnamed, never follows symlinks and stays on
// the volume's filesystem.
func drop(w io.Writer, root string, names []string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("mirror root %s is not a directory", root)
	}
	for _, name := range names {
		if !mirrorName.MatchString(name) {
			return fmt.Errorf("invalid mirror name %q", name)
		}
	}
	dev, u := device(info), mirrorUsage{Mirrors: map[string]int64{}}
	for _, name := range names {
		for _, suffix := range []string{"", ".new", ".old"} {
			u.remove(filepath.Join(root, name+suffix), dev)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && mirrorName.MatchString(e.Name()) {
			u.Mirrors[e.Name()] = measure(filepath.Join(root, e.Name()), dev)
		}
	}
	u.Size = measure(root, dev)
	return json.NewEncoder(w).Encode(u)
}
