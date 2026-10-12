package docker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestCreateArgsPerRuntime(t *testing.T) {
	image := "example.invalid/agent@sha256:" + strings.Repeat("a", 64)
	runc := (&Backend{Image: image, Owner: "o", Command: []string{"/bin/sh"}}).createArgs("n", DefaultResources)
	if !slices.Contains(runc, "--runtime=runc") || !slices.Contains(runc, "--user="+AttemptUser) || slices.ContainsFunc(runc, func(a string) bool { return strings.HasPrefix(a, "--tmpfs=/run") }) {
		t.Fatal("runc attempt", runc)
	}
	sysbox := (&Backend{Image: image, Owner: "o", Command: []string{"/bin/sh"}, Runtime: Sysbox}).createArgs("n", SysboxResources.InPool("multica_attempts.slice", 0))
	for _, want := range []string{"--runtime=sysbox-runc", "--user=0:0", "--tmpfs=/run:" + runTmpfs, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges=true",
		"--cgroupns=private", "--cgroup-parent=multica_attempts.slice", "--pids-limit=2048", "--ulimit=nofile=65536:65536"} {
		if !slices.Contains(sysbox, want) {
			t.Errorf("sysbox attempt lacks %s", want)
		}
	}
	if slices.ContainsFunc(sysbox, func(a string) bool { return strings.HasPrefix(a, "--memory") }) {
		t.Error("pool attempt without a ceiling has a memory limit")
	}
	ceiling := SysboxResources.InPool("multica_attempts.slice", 6<<30).args()
	if !slices.Contains(ceiling, "--memory=6442450944") || !slices.Contains(ceiling, "--memory-swap=-1") {
		t.Error("pool ceiling must allow swap", ceiling)
	}
	if args := DefaultResources.args(); !slices.Contains(args, "--memory-swap=4294967296") || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--cgroup-parent") }) {
		t.Error("fixed limits changed without a pool", args)
	}
	entry := (&Projected{Backend: Backend{Image: image, Runtime: Sysbox}, DinD: DinD{RegistryMirrors: []string{"http://mirror:5000"}, InsecureRegistries: []string{"mirror:5000"}}}).entrypoint()
	if !slices.Equal(entry, []string{"--entrypoint", HelperDir + "/sandbox-helper", image, "dind", "--registry-mirror=http://mirror:5000", "--insecure-registry=mirror:5000"}) {
		t.Error(entry)
	}
	if entry := (&Projected{Backend: Backend{Image: image}}).entrypoint(); !slices.Equal(entry, []string{"--entrypoint", "/bin/sh", image, "-c", "exec sleep 86400"}) {
		t.Error(entry)
	}
}

// sysboxPolicy is safePolicy as created for a sysbox attempt in the pool.
func sysboxPolicy(t *testing.T, change func(config, host map[string]any)) []byte {
	var fixture []map[string]any
	if err := json.Unmarshal([]byte(safePolicy), &fixture); err != nil {
		t.Fatal(err)
	}
	config, host := fixture[0]["Config"].(map[string]any), fixture[0]["HostConfig"].(map[string]any)
	config["User"], host["Runtime"], host["CgroupParent"], host["Memory"], host["MemorySwap"] = "0:0", Sysbox, "multica_attempts.slice", 0, 0
	host["Tmpfs"].(map[string]any)["/run"] = runTmpfs
	if change != nil {
		change(config, host)
	}
	data, _ := json.Marshal(fixture)
	return data
}

func TestSysboxPolicy(t *testing.T) {
	want := expected{network: "none", limits: probeResources.InPool("multica_attempts.slice", 0), runtime: Sysbox}
	if err := checkExpectedPolicy(sysboxPolicy(t, nil), want); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(config, host map[string]any){
		"runc runtime":     func(_, h map[string]any) { h["Runtime"] = "runc" },
		"attempt user":     func(c, _ map[string]any) { c["User"] = AttemptUser },
		"no /run":          func(_, h map[string]any) { delete(h["Tmpfs"].(map[string]any), "/run") },
		"executable /run":  func(_, h map[string]any) { h["Tmpfs"].(map[string]any)["/run"] = "rw,nosuid,nodev,size=67108864" },
		"other parent":     func(_, h map[string]any) { h["CgroupParent"] = "system.slice" },
		"no parent":        func(_, h map[string]any) { h["CgroupParent"] = "" },
		"memory limit":     func(_, h map[string]any) { h["Memory"] = 1 << 30 },
		"swap":             func(_, h map[string]any) { h["MemorySwap"] = -1 },
		"privileged":       func(_, h map[string]any) { h["Privileged"] = true },
		"capability added": func(_, h map[string]any) { h["CapAdd"] = []string{"SYS_ADMIN"} },
		"writable root":    func(_, h map[string]any) { h["ReadonlyRootfs"] = false },
	} {
		if checkExpectedPolicy(sysboxPolicy(t, change), want) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A runc expectation refuses a sysbox container and the reverse.
	if checkExpectedPolicy(sysboxPolicy(t, nil), expected{network: "none", limits: want.limits}) == nil {
		t.Error("sysbox container accepted as runc")
	}
	if checkPolicy([]byte(strings.Replace(safePolicy, `"Runtime":"runc"`, `"Runtime":"sysbox-runc"`, 1))) == nil {
		t.Error("runc policy accepted sysbox-runc")
	}
	ceiling := want
	ceiling.limits = probeResources.InPool("multica_attempts.slice", 1<<30)
	if checkExpectedPolicy(sysboxPolicy(t, func(_, h map[string]any) { h["Memory"], h["MemorySwap"] = 1<<30, -1 }), ceiling) != nil {
		t.Error("pool ceiling with swap refused")
	}
	if checkExpectedPolicy(sysboxPolicy(t, func(_, h map[string]any) { h["Memory"], h["MemorySwap"] = 1<<30, 1<<30 }), ceiling) == nil {
		t.Error("pool ceiling without swap accepted")
	}
}

// facts is what `cat /proc/self/status /proc/self/uid_map /proc/self/mountinfo` prints.
func facts(capEff, nnp, uidMap, cgroup, docker string) []byte {
	return []byte("Name:\tcat\nVmPeak:\t  1234 kB\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t" + capEff +
		"\nCapBnd:\t000001ffffffffff\nCapAmb:\t0000000000000000\nNoNewPrivs:\t" + nnp + "\nSpeculation_Store_Bypass:\tthread vulnerable\n" + uidMap +
		"\n1062 999 0:96 / / ro,relatime master:1 - overlay overlay rw,lowerdir=/x\n" +
		"1070 1062 0:100 / /sys/fs/cgroup " + cgroup + ",nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw\n" +
		"1080 1062 253:1 /var/lib/sysbox/docker/x /var/lib/docker " + docker + ",relatime - ext4 /dev/vda1 rw\n")
}

func TestRuntimeFactsReplaceCapDropTrust(t *testing.T) {
	sysbox := "         0     100000      65536"
	if err := runtimeFacts(facts("0000000000000000", "1", sysbox, "rw", "rw"), true); err != nil {
		t.Fatal(err)
	}
	if err := runtimeFacts(facts("0000000000000000", "1", "         0          0 4294967295", "ro", "ro"), false); err != nil {
		t.Fatal("runc", err)
	}
	for name, out := range map[string][]byte{
		"capabilities":      facts("000001ffffffffff", "1", sysbox, "rw", "rw"),
		"no NNP":            facts("0000000000000000", "0", sysbox, "rw", "rw"),
		"identity map":      facts("0000000000000000", "1", "         0          0 4294967295", "rw", "rw"),
		"read-only cgroup":  facts("0000000000000000", "1", sysbox, "ro", "rw"),
		"read-only engine":  facts("0000000000000000", "1", sysbox, "rw", "ro"),
		"no user namespace": facts("0000000000000000", "1", "", "rw", "rw"),
		"writable root":     []byte(strings.Replace(string(facts("0000000000000000", "1", sysbox, "rw", "rw")), "/ / ro,", "/ / rw,", 1)),
		"empty":             nil,
	} {
		if runtimeFacts(out, true) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
