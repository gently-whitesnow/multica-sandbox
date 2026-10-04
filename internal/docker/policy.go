package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
)

func (r *run) check(ctx context.Context) error {
	data, err := command(ctx, "inspect", r.name)
	if err != nil {
		return err
	}
	return checkPolicy(data)
}

func checkPolicy(data []byte) error {
	var containers []struct {
		Config struct {
			User, WorkingDir string
			Volumes          map[string]json.RawMessage
			Healthcheck      struct{ Test []string }
		}
		HostConfig struct {
			NetworkMode, Runtime, PidMode, IpcMode, CgroupnsMode string
			ReadonlyRootfs, Privileged, PublishAllPorts          bool
			CapAdd, CapDrop, SecurityOpt, Binds, VolumesFrom     []string
			Devices, DeviceRequests                              []json.RawMessage
			Memory, MemorySwap, NanoCpus, ShmSize                int64
			PidsLimit                                            int
			Tmpfs                                                map[string]string
			RestartPolicy                                        struct{ Name string }
			LogConfig                                            struct{ Type string }
		}
		Mounts []json.RawMessage
	}
	if err := json.Unmarshal(data, &containers); err != nil {
		return err
	}
	if len(containers) != 1 {
		return fmt.Errorf("unexpected inspect response")
	}
	c := containers[0]
	h := c.HostConfig
	if c.Config.User != "65532:65532" || c.Config.WorkingDir != "/workspace" || len(c.Config.Volumes) != 0 ||
		!reflect.DeepEqual(c.Config.Healthcheck.Test, []string{"NONE"}) ||
		h.NetworkMode != "none" || h.Runtime != "runc" || h.PidMode != "" || h.IpcMode != "private" || h.CgroupnsMode != "private" ||
		!h.ReadonlyRootfs || h.Privileged || h.PublishAllPorts || len(h.CapAdd) != 0 || !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) ||
		!reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges=true"}) || len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(c.Mounts) != 0 ||
		h.Memory != 128*1024*1024 || h.MemorySwap != h.Memory || h.NanoCpus != 500000000 || h.PidsLimit != 64 || h.ShmSize != 8*1024*1024 ||
		h.RestartPolicy.Name != "no" || h.LogConfig.Type != "none" ||
		!reflect.DeepEqual(h.Tmpfs, map[string]string{"/workspace": "rw,nosuid,nodev,size=67108864,mode=1777", "/tmp": "rw,noexec,nosuid,nodev,size=16777216,mode=1777"}) {
		return fmt.Errorf("created container does not satisfy offline policy")
	}
	return nil
}
