package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

func (r *run) check(ctx context.Context) error {
	data, err := command(ctx, "inspect", r.name)
	if err != nil {
		return err
	}
	return checkPolicy(data)
}

func checkPolicy(data []byte) error {
	return checkExpectedPolicy(data, "none", 128*1024*1024, 64, "rw,nosuid,nodev,size=67108864,mode=1777", nil, "")
}

func checkProjectedPolicy(data []byte, network string, bundles []Bundle, path string) error {
	return checkExpectedPolicy(data, network, 1024*1024*1024, 256, "rw,nosuid,nodev,size=268435456,mode=1777", bundles, path)
}

type mount struct {
	Type, Name, Destination string
	RW                      bool
}

func checkExpectedPolicy(data []byte, network string, memory int64, pids int, workspace string, bundles []Bundle, path string) error {
	var containers []struct {
		Config struct {
			User, WorkingDir string
			Env              []string
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
			Mounts                                               []map[string]any
		}
		Mounts []mount
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
		h.NetworkMode != network || h.Runtime != "runc" || h.PidMode != "" || h.IpcMode != "private" || h.CgroupnsMode != "private" ||
		!h.ReadonlyRootfs || h.Privileged || h.PublishAllPorts || len(h.CapAdd) != 0 || !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) ||
		!reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges=true"}) || len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 ||
		h.Memory != memory || h.MemorySwap != h.Memory || h.NanoCpus != 500000000 || h.PidsLimit != pids || h.ShmSize != 8*1024*1024 ||
		h.RestartPolicy.Name != "no" || h.LogConfig.Type != "none" ||
		!reflect.DeepEqual(h.Tmpfs, map[string]string{"/workspace": workspace, "/tmp": "rw,noexec,nosuid,nodev,size=16777216,mode=1777"}) {
		return fmt.Errorf("created container does not satisfy offline policy")
	}
	// Exactly the requested read-only image mounts, without options such as a subpath.
	ok := len(h.Mounts) == len(bundles) && len(c.Mounts) == len(bundles)
	for i, bundle := range bundles {
		ok = ok && reflect.DeepEqual(h.Mounts[i], map[string]any{"Type": "image", "Source": bundle.Image, "Target": bundle.Target})
		ok = ok && slices.Contains(c.Mounts, mount{"image", bundle.Image, bundle.Target, false})
	}
	if len(bundles) > 0 {
		paths := []string{}
		for _, env := range c.Config.Env {
			if strings.HasPrefix(env, "PATH=") {
				paths = append(paths, env)
			}
		}
		ok = ok && reflect.DeepEqual(paths, []string{"PATH=" + path})
	}
	if !ok {
		return fmt.Errorf("created container mounts do not match the controller bundles")
	}
	return nil
}
