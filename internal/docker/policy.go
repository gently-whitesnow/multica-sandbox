package docker

import (
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"

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
	return checkExpectedPolicy(data, expected{network: "none", limits: probeResources})
}

// expected is the policy of a created container: its network, resources, read-only image
// mounts with their PATH, exactly the workdir volume when one is named, the loopback host
// entries and the runtime (empty for runc).
type expected struct {
	network      string
	limits       Resources
	bundles      []Bundle
	path, volume string
	hosts        []string
	runtime      string
}

// loopbackHosts are the --add-host entries for names served on attempt loopback.
func loopbackHosts(names []string) []string {
	hosts := []string{}
	for _, name := range names {
		hosts = append(hosts, name+":127.0.0.1")
	}
	return hosts
}

type mount struct {
	Type, Name, Destination string
	RW                      bool
}

func checkExpectedPolicy(data []byte, want expected) error {
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
			ExtraHosts                                           []string
			Devices, DeviceRequests                              []json.RawMessage
			Memory, MemorySwap, NanoCpus, ShmSize                int64
			PidsLimit                                            int
			Tmpfs                                                map[string]string
			Ulimits                                              []ulimit
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
	runtime, user := "runc", AttemptUser
	if want.runtime == Sysbox {
		runtime, user = Sysbox, "0:0"
	}
	bundles, path, volume := want.bundles, want.path, want.volume
	if c.Config.User != user || c.Config.WorkingDir != "/workspace" || len(c.Config.Volumes) != 0 ||
		!reflect.DeepEqual(c.Config.Healthcheck.Test, []string{"NONE"}) ||
		h.NetworkMode != want.network || h.Runtime != runtime || h.PidMode != "" || h.IpcMode != "private" || h.CgroupnsMode != "private" ||
		!h.ReadonlyRootfs || h.Privileged || h.PublishAllPorts || len(h.CapAdd) != 0 || !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) ||
		!reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges=true"}) || len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 ||
		!want.limits.matches(h.Memory, h.MemorySwap, h.NanoCpus, h.ShmSize, h.PidsLimit, h.Tmpfs, h.Ulimits, runtime == Sysbox) ||
		h.RestartPolicy.Name != "no" || h.LogConfig.Type != "none" || !slices.Equal(h.ExtraHosts, loopbackHosts(want.hosts)) {
		return fmt.Errorf("created container does not satisfy offline policy")
	}
	// Exactly the requested read-only image mounts, without options such as a subpath,
	// and the writable workdir volume without copy-up from the image.
	mounts := len(bundles)
	if volume != "" {
		mounts++
	}
	ok := len(h.Mounts) == mounts && len(c.Mounts) == mounts
	for i, bundle := range bundles {
		ok = ok && reflect.DeepEqual(h.Mounts[i], map[string]any{"Type": "image", "Source": bundle.Image, "Target": bundle.Target})
		ok = ok && slices.Contains(c.Mounts, mount{"image", bundle.Image, bundle.Target, false})
	}
	if ok && volume != "" {
		ok = reflect.DeepEqual(h.Mounts[len(bundles)], map[string]any{"Type": "volume", "Source": volume, "Target": repo.WorkDir, "VolumeOptions": map[string]any{"NoCopy": true}})
		ok = ok && slices.Contains(c.Mounts, mount{"volume", volume, repo.WorkDir, true})
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
