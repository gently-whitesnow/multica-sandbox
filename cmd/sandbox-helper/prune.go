package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Rebuildable outputs of an idle session volume (ADR 0034): the volume HOME's tool
// caches, the attempt TMPDIR, and build directories of checkouts. .NET bin and obj go
// only beside a project file.
var (
	pruneCaches   = []string{".multica-sandbox/home/.cache", ".multica-sandbox/home/.npm", ".multica-sandbox/home/.nuget/packages", ".multica-sandbox/home/go/pkg/mod", ".multica-sandbox/tmp"}
	pruneBuilds   = map[string]bool{"node_modules": true, ".next": true, ".nuxt": true, ".turbo": true}
	pruneProjects = []string{".csproj", ".fsproj", ".vbproj"}
)

// pruneDepth bounds the search for build directories below the volume root.
const pruneDepth = 8

type usage struct {
	Freed  int64 `json:"freed"`
	Size   int64 `json:"size"`
	Failed int   `json:"failed"`
}

// report prunes root when asked, measures what stays and prints the usage as JSON.
func report(w io.Writer, root string, remove bool) error {
	u, err := prune(root, remove)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(u)
}

// prune never follows symlinks, leaves root's filesystem or enters .git; entries it
// cannot remove are counted, not fatal.
func prune(root string, remove bool) (usage, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return usage{}, fmt.Errorf("prune root %s is not a directory", root)
	}
	dev, u := device(info), usage{}
	if remove {
		for _, rel := range pruneCaches {
			if path, ok := realParent(root, rel); ok {
				u.remove(path, dev)
			}
		}
		u.search(root, dev, 0)
	}
	u.Size = measure(root, dev)
	return u, nil
}

// realParent joins rel to root when every directory on the way is a real directory.
func realParent(root, rel string) (string, bool) {
	path := root
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		path = filepath.Join(path, part)
		if info, err := os.Lstat(path); err != nil || !info.IsDir() {
			return "", false
		}
	}
	return filepath.Join(path, parts[len(parts)-1]), true
}

func (u *usage) search(dir string, dev uint64, depth int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		u.Failed++
		return
	}
	project := false
	for _, e := range entries {
		for _, ext := range pruneProjects {
			project = project || e.Type().IsRegular() && strings.HasSuffix(e.Name(), ext)
		}
	}
	for _, e := range entries {
		name, path := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case !e.IsDir() || name == ".git" || depth == 0 && (name == ".multica-sandbox" || name == ".multica"):
		case pruneBuilds[name] || project && (name == "bin" || name == "obj"):
			u.remove(path, dev)
		case depth+1 < pruneDepth:
			if info, err := os.Lstat(path); err == nil && info.IsDir() && device(info) == dev {
				u.search(path, dev, depth+1)
			}
		}
	}
}

// remove deletes a tree bottom-up. Directories it owns but cannot write, like the Go
// module cache, are made writable first.
func (u *usage) remove(path string, dev uint64) {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			u.Failed++
		}
		return
	}
	if info.IsDir() {
		if device(info) != dev {
			return
		}
		if perm := info.Mode().Perm(); perm&0700 != 0700 {
			_ = os.Chmod(path, perm|0700)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			u.Failed++
			return
		}
		for _, e := range entries {
			u.remove(filepath.Join(path, e.Name()), dev)
		}
	}
	if err := os.Remove(path); err != nil {
		u.Failed++
		return
	}
	u.Freed += allocated(info)
}

// measure sums allocated bytes on root's filesystem without following symlinks.
func measure(root string, dev uint64) int64 {
	var size int64
	_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if e.IsDir() && device(info) != dev {
			return filepath.SkipDir
		}
		size += allocated(info)
		return nil
	})
	return size
}

func device(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

func allocated(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}
