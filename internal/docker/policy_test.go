package docker

import (
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"

	"encoding/json"
	"strings"
	"testing"
)

const safePolicy = `[{"Config":{"User":"65532:65532","WorkingDir":"/workspace","Healthcheck":{"Test":["NONE"]}},"HostConfig":{
"NetworkMode":"none","Runtime":"runc","IpcMode":"private","CgroupnsMode":"private","ReadonlyRootfs":true,
"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges=true"],"Memory":134217728,"MemorySwap":134217728,
"NanoCpus":500000000,"PidsLimit":64,"ShmSize":8388608,"RestartPolicy":{"Name":"no"},"LogConfig":{"Type":"none"},
"Tmpfs":{"/workspace":"rw,exec,nosuid,nodev,size=67108864,mode=1777","/tmp":"rw,noexec,nosuid,nodev,size=16777216,mode=1777"}}}]`

func TestRejectWeakenedPolicy(t *testing.T) {
	if err := checkPolicy([]byte(safePolicy)); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]map[string]any{
		"HostConfig": {"NetworkMode": "bridge", "Privileged": true, "PidMode": "host", "ReadonlyRootfs": false,
			"Binds": []string{"/:/host"}, "Memory": 0, "PidsLimit": 0, "CapAdd": []string{"SYS_ADMIN"}, "SecurityOpt": []string{},
			"RestartPolicy": map[string]string{"Name": "always"}, "Tmpfs": map[string]string{"/workspace": "rw"},
			"ExtraHosts": []string{"api.github.com:127.0.0.1"}},
		"Config": {"User": "0", "Volumes": map[string]any{"/data": map[string]any{}}, "Healthcheck": map[string]any{"Test": []string{"CMD", "/bin/evil"}}},
	}
	for section, fields := range mutations {
		for key, value := range fields {
			t.Run(section+"/"+key, func(t *testing.T) {
				var fixture []map[string]any
				if err := json.Unmarshal([]byte(safePolicy), &fixture); err != nil {
					t.Fatal(err)
				}
				fixture[0][section].(map[string]any)[key] = value
				data, _ := json.Marshal(fixture)
				if err := checkPolicy(data); err == nil {
					t.Fatal("unsafe configuration accepted")
				}
			})
		}
	}
}

func TestAcceptOnlyBundleMounts(t *testing.T) {
	cli := Bundle{Image: "example.invalid/cli@sha256:" + strings.Repeat("a", 64), Target: "/opt/multica-sandbox/multica"}
	path := cli.Target + "/bin:/usr/bin:/bin"
	check := func(bundles []Bundle, change func(c, config, host map[string]any), volume ...string) error {
		var fixture []map[string]any
		if err := json.Unmarshal([]byte(safePolicy), &fixture); err != nil {
			t.Fatal(err)
		}
		c := fixture[0]
		config, host := c["Config"].(map[string]any), c["HostConfig"].(map[string]any)
		config["Env"] = []string{"PATH=" + path, "LANG=C"}
		host["Mounts"] = []any{map[string]any{"Type": "image", "Source": cli.Image, "Target": cli.Target}}
		c["Mounts"] = []any{map[string]any{"Type": "image", "Name": cli.Image, "Source": "/var/lib/docker/rootfs/overlayfs/x", "Destination": cli.Target, "Mode": "", "RW": false, "Propagation": "rprivate"}}
		if change != nil {
			change(c, config, host)
		}
		data, _ := json.Marshal(fixture)
		return checkExpectedPolicy(data, "none", 128*1024*1024, 64, "rw,exec,nosuid,nodev,size=67108864,mode=1777", bundles, path, strings.Join(volume, ""), nil)
	}
	if err := check([]Bundle{cli}, nil); err != nil {
		t.Fatal(err)
	}
	if check(nil, nil) == nil {
		t.Fatal("image mount accepted without a configured bundle")
	}
	other := map[string]any{"Type": "image", "Name": cli.Image, "Destination": "/opt/multica-sandbox/other", "RW": false}
	for name, change := range map[string]func(c, config, host map[string]any){
		"missing":  func(c, _, host map[string]any) { c["Mounts"], host["Mounts"] = nil, nil },
		"writable": func(c, _, _ map[string]any) { c["Mounts"].([]any)[0].(map[string]any)["RW"] = true },
		"wrong digest": func(c, _, _ map[string]any) {
			c["Mounts"].([]any)[0].(map[string]any)["Name"] = strings.Replace(cli.Image, "aaaa", "bbbb", 1)
		},
		"wrong target": func(c, _, _ map[string]any) { c["Mounts"].([]any)[0].(map[string]any)["Destination"] = "/usr/local" },
		"bind":         func(c, _, _ map[string]any) { c["Mounts"].([]any)[0].(map[string]any)["Type"] = "bind" },
		"extra":        func(c, _, _ map[string]any) { c["Mounts"] = append(c["Mounts"].([]any), other) },
		"subpath": func(_, _, host map[string]any) {
			host["Mounts"].([]any)[0].(map[string]any)["ImageOptions"] = map[string]any{"Subpath": "bin"}
		},
		"host extra": func(_, _, host map[string]any) {
			host["Mounts"] = append(host["Mounts"].([]any), map[string]any{"Type": "volume"})
		},
		"path missing":  func(_, config, _ map[string]any) { config["Env"] = []string{"LANG=C"} },
		"path shadowed": func(_, config, _ map[string]any) { config["Env"] = []string{"PATH=/workspace/bin:" + path} },
		"path twice":    func(_, config, _ map[string]any) { config["Env"] = []string{"PATH=" + path, "PATH=/usr/bin"} },
	} {
		t.Run(name, func(t *testing.T) {
			if check([]Bundle{cli}, change) == nil {
				t.Fatal("unexpected mount configuration accepted")
			}
		})
	}
}

func TestAcceptOnlyWorkdirVolume(t *testing.T) {
	cli := Bundle{Image: "example.invalid/cli@sha256:" + strings.Repeat("a", 64), Target: "/opt/multica-sandbox/multica"}
	path := cli.Target + "/bin:/usr/bin:/bin"
	const volume = "multica-sandbox-0123-work"
	check := func(change func(c, host map[string]any)) error {
		var fixture []map[string]any
		if err := json.Unmarshal([]byte(safePolicy), &fixture); err != nil {
			t.Fatal(err)
		}
		c := fixture[0]
		host := c["HostConfig"].(map[string]any)
		c["Config"].(map[string]any)["Env"] = []string{"PATH=" + path}
		host["Mounts"] = []any{map[string]any{"Type": "image", "Source": cli.Image, "Target": cli.Target},
			map[string]any{"Type": "volume", "Source": volume, "Target": repo.WorkDir, "VolumeOptions": map[string]any{"NoCopy": true}}}
		c["Mounts"] = []any{map[string]any{"Type": "image", "Name": cli.Image, "Destination": cli.Target, "RW": false},
			map[string]any{"Type": "volume", "Name": volume, "Source": "/var/lib/docker/volumes/x/_data", "Destination": repo.WorkDir, "Driver": "local", "RW": true}}
		if change != nil {
			change(c, host)
		}
		data, _ := json.Marshal(fixture)
		return checkExpectedPolicy(data, "none", 128*1024*1024, 64, "rw,exec,nosuid,nodev,size=67108864,mode=1777", []Bundle{cli}, path, volume, nil)
	}
	if err := check(nil); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(c, host map[string]any){
		"missing": func(c, host map[string]any) {
			c["Mounts"], host["Mounts"] = c["Mounts"].([]any)[:1], host["Mounts"].([]any)[:1]
		},
		"copy-up": func(_, host map[string]any) { delete(host["Mounts"].([]any)[1].(map[string]any), "VolumeOptions") },
		"other":   func(_, host map[string]any) { host["Mounts"].([]any)[1].(map[string]any)["Source"] = "shared" },
		"bind":    func(c, _ map[string]any) { c["Mounts"].([]any)[1].(map[string]any)["Type"] = "bind" },
		"target":  func(c, _ map[string]any) { c["Mounts"].([]any)[1].(map[string]any)["Destination"] = "/workspace" },
		"subpath": func(_, host map[string]any) {
			host["Mounts"].([]any)[1].(map[string]any)["VolumeOptions"].(map[string]any)["Subpath"] = "x"
		},
		"read-only": func(c, _ map[string]any) { c["Mounts"].([]any)[1].(map[string]any)["RW"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			if check(change) == nil {
				t.Fatal("unexpected workdir volume accepted")
			}
		})
	}
}

func TestAcceptOnlyLoopbackForgeHosts(t *testing.T) {
	check := func(extra []string, hosts []string) error {
		var fixture []map[string]any
		if err := json.Unmarshal([]byte(safePolicy), &fixture); err != nil {
			t.Fatal(err)
		}
		fixture[0]["HostConfig"].(map[string]any)["ExtraHosts"] = extra
		data, _ := json.Marshal(fixture)
		return checkExpectedPolicy(data, "none", 128*1024*1024, 64, "rw,exec,nosuid,nodev,size=67108864,mode=1777", nil, "", "", hosts)
	}
	if err := check([]string{"api.github.com:127.0.0.1"}, []string{"api.github.com"}); err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string][]string{"missing": nil, "remote": {"api.github.com:10.0.0.1"}, "extra": {"api.github.com:127.0.0.1", "github.com:127.0.0.1"}} {
		if check(extra, []string{"api.github.com"}) == nil {
			t.Errorf("%s host entries accepted", name)
		}
	}
}
