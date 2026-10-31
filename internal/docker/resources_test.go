package docker

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestResourceSpecDefaultsAndBounds(t *testing.T) {
	if r, err := (*ResourceSpec)(nil).Resolve(""); err != nil || r != DefaultResources {
		t.Fatal("unset resources differ from the defaults", r, err)
	}
	r, err := (&ResourceSpec{CPUs: 4, Memory: "6g", Pids: 4096, NoFile: 65536, WorkspaceSize: "2g", TmpSize: "512m", ShmSize: "1g"}).Resolve("")
	want := Resources{NanoCPUs: 4e9, Memory: 6 << 30, Pids: 4096, NoFile: 65536, Workspace: 2 << 30, Tmp: 512 << 20, Shm: 1 << 30}
	if err != nil || r != want {
		t.Fatal("configured resources not applied", r, err)
	}
	if r, _ := (&ResourceSpec{Memory: "8G"}).Resolve(""); r.Memory != 8<<30 || r.Pids != DefaultResources.Pids {
		t.Fatal("partial spec did not keep defaults", r)
	}
	for name, spec := range map[string]ResourceSpec{
		"cpus": {CPUs: 0.01}, "negative cpus": {CPUs: -1}, "memory unit": {Memory: "4gb"}, "tiny memory": {Memory: "1m"},
		"zero": {Memory: "0"}, "overflow": {Memory: "99999999999999g"}, "pids": {Pids: 8}, "nofile": {NoFile: 1 << 21}, "tmp": {TmpSize: "-1m"},
	} {
		if _, err := spec.Resolve(""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPolicyMatchesConfiguredResources(t *testing.T) {
	limits := Resources{NanoCPUs: 25e8, Memory: 3 << 30, Pids: 512, NoFile: 2048, Workspace: 1 << 30, Tmp: 64 << 20, Shm: 32 << 20}
	args := limits.args()
	if !slices.Contains(args, "--cpus=2.5") || !slices.Contains(args, "--ulimit=nofile=2048:2048") || !slices.Contains(args, "--memory-swap=3221225472") {
		t.Fatal("create arguments differ from the resources", args)
	}
	var fixture []map[string]any
	if err := json.Unmarshal([]byte(safePolicy), &fixture); err != nil {
		t.Fatal(err)
	}
	host := fixture[0]["HostConfig"].(map[string]any)
	host["Memory"], host["MemorySwap"], host["NanoCpus"], host["PidsLimit"], host["ShmSize"] = limits.Memory, limits.Memory, limits.NanoCPUs, limits.Pids, limits.Shm
	host["Ulimits"] = []any{map[string]any{"Name": "nofile", "Soft": 2048, "Hard": 2048}}
	host["Tmpfs"] = map[string]string{"/workspace": limits.workspaceTmpfs(), "/tmp": limits.tmpTmpfs()}
	data, _ := json.Marshal(fixture)
	if err := checkExpectedPolicy(data, expected{network: "none", limits: limits}); err != nil {
		t.Fatal(err)
	}
	// A container created with other limits (the defaults) fails the configured policy.
	if checkExpectedPolicy(data, expected{network: "none", limits: DefaultResources}) == nil {
		t.Fatal("inspect accepted against different resources")
	}
	host["Ulimits"] = []any{map[string]any{"Name": "nofile", "Soft": 2048, "Hard": 4096}}
	data, _ = json.Marshal(fixture)
	if checkExpectedPolicy(data, expected{network: "none", limits: limits}) == nil {
		t.Fatal("raised hard nofile accepted")
	}
}

func TestTreeArchiveHoldsOnlyCleanRelativeFiles(t *testing.T) {
	var archive bytes.Buffer
	if err := writeTar(&archive, map[string][]byte{"b/c/d.md": []byte("d"), "a.md": []byte("a"), "b/e.md": nil}, 1<<20); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for r := tar.NewReader(&archive); ; {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf("%c %s %o", h.Typeflag, h.Name, h.Mode))
	}
	if want := []string{"0 a.md 644", "5 b/ 755", "5 b/c/ 755", "0 b/c/d.md 644", "0 b/e.md 644"}; !slices.Equal(entries, want) {
		t.Fatalf("entries %v", entries)
	}
	for _, name := range []string{"", ".", "..", "../x", "/x", "a/../b", "a//b", "a/", `a\b`, "a\x00b"} {
		if writeTar(io.Discard, map[string][]byte{name: nil}, 1<<20) == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if writeTar(io.Discard, map[string][]byte{"a": make([]byte, 11)}, 10) == nil {
		t.Error("oversized tree accepted")
	}
}

func TestSharedCPUIsExplicitAndHasNoPerAttemptQuota(t *testing.T) {
	r, err := (&ResourceSpec{CPUMode: "shared"}).Resolve(Sysbox)
	if err != nil || r.NanoCPUs != 0 {
		t.Fatal(r, err)
	}
	for _, arg := range r.args() {
		if strings.HasPrefix(arg, "--cpus=") {
			t.Fatal("shared CPU has a quota", arg)
		}
	}
	if _, err := (&ResourceSpec{CPUMode: "shared", CPUs: 2}).Resolve(Sysbox); err == nil {
		t.Fatal("conflicting CPU modes accepted")
	}
	if _, err := (&ResourceSpec{CPUMode: "automatic"}).Resolve(Sysbox); err == nil {
		t.Fatal("unknown CPU mode accepted")
	}
	if r, _ := (*ResourceSpec)(nil).Resolve(Sysbox); r.NanoCPUs != 2e9 {
		t.Fatal("legacy CPU default changed")
	}
}
