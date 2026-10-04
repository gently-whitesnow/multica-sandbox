package docker

import (
	"encoding/json"
	"testing"
)

const safePolicy = `[{"Config":{"User":"65532:65532","WorkingDir":"/workspace","Healthcheck":{"Test":["NONE"]}},"HostConfig":{
"NetworkMode":"none","Runtime":"runc","IpcMode":"private","CgroupnsMode":"private","ReadonlyRootfs":true,
"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges=true"],"Memory":134217728,"MemorySwap":134217728,
"NanoCpus":500000000,"PidsLimit":64,"ShmSize":8388608,"RestartPolicy":{"Name":"no"},"LogConfig":{"Type":"none"},
"Tmpfs":{"/workspace":"rw,nosuid,nodev,size=67108864,mode=1777","/tmp":"rw,noexec,nosuid,nodev,size=16777216,mode=1777"}}}]`

func TestRejectWeakenedPolicy(t *testing.T) {
	if err := checkPolicy([]byte(safePolicy)); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]map[string]any{
		"HostConfig": {"NetworkMode": "bridge", "Privileged": true, "PidMode": "host", "ReadonlyRootfs": false,
			"Binds": []string{"/:/host"}, "Memory": 0, "PidsLimit": 0, "CapAdd": []string{"SYS_ADMIN"}, "SecurityOpt": []string{},
			"RestartPolicy": map[string]string{"Name": "always"}, "Tmpfs": map[string]string{"/workspace": "rw"}},
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
