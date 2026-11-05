package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDropRemovesOnlyNamedMirrors(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	kept, gone, unknown := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	for _, name := range []string{kept, kept + ".new", gone, gone + ".new", gone + ".old", unknown, "junk"} {
		if err := os.MkdirAll(filepath.Join(root, name, "objects", "pack"), 0700); err != nil ||
			os.WriteFile(filepath.Join(root, name, "objects", "pack", "p.pack"), make([]byte, 8192), 0444) != nil {
			t.Fatal(err)
		}
	}
	// Pack files are read-only; removal needs only the writable directory.
	if err := os.WriteFile(filepath.Join(outside, "file"), nil, 0600); err != nil || os.Symlink(outside, filepath.Join(root, gone, "link")) != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := drop(&out, root, []string{gone}); err != nil {
		t.Fatal(err)
	}
	var u mirrorUsage
	if err := json.Unmarshal(out.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	// The named mirror and its rebuilds go; unnamed mirrors are measured, other entries stay.
	for name, want := range map[string]bool{kept: true, kept + ".new": true, unknown: true, "junk": true, gone: false, gone + ".new": false, gone + ".old": false} {
		if _, err := os.Lstat(filepath.Join(root, name)); (err == nil) != want {
			t.Errorf("%s present=%v", name, err == nil)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); err != nil {
		t.Fatal("drop followed a symbolic link")
	}
	if len(u.Mirrors) != 2 || u.Mirrors[kept] < 8192 || u.Mirrors[unknown] < 8192 || u.Freed < 3*8192 || u.Size < u.Mirrors[kept]+u.Mirrors[unknown] || u.Failed != 0 {
		t.Fatalf("usage %+v", u)
	}
	for _, bad := range []string{"..", "../" + kept, strings.ToUpper(kept), "junk"} {
		if err := drop(&out, root, []string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "junk")); err != nil {
		t.Fatal("invalid name removed an entry")
	}
}
