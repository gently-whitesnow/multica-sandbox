package service

import (
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
)

func TestAttemptRuntimeConfiguration(t *testing.T) {
	helper := "example.invalid/helper@sha256:" + strings.Repeat("b", 64)
	if runtime, _, err := attemptRuntime(&OpenCodeConfig{}); runtime != "" || err != nil {
		t.Fatal("runc is not the default", runtime, err)
	}
	runtime, dind, err := attemptRuntime(&OpenCodeConfig{Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"http://mirror:5000"}, InsecureRegistries: []string{"mirror:5000", "10.0.0.0/8"}, TestcontainersRyukDisabled: true}})
	if runtime != docker.Sysbox || err != nil || dind.ReadyTimeout != 30*time.Second || !dind.RyukDisabled || len(dind.InsecureRegistries) != 2 {
		t.Fatal(runtime, dind, err)
	}
	for name, c := range map[string]OpenCodeConfig{
		"unknown runtime":   {Runtime: "kata", Helper: helper},
		"dind under runc":   {DinD: &DinDConfig{}},
		"sysbox no helper":  {Runtime: "sysbox-runc"},
		"mirror path":       {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"http://mirror:5000/v2"}}},
		"mirror userinfo":   {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"https://u:p@mirror"}}},
		"mirror scheme":     {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"mirror:5000"}}},
		"insecure flag":     {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{InsecureRegistries: []string{"--debug"}}},
		"insecure spaces":   {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{InsecureRegistries: []string{"a b"}}},
		"short timeout":     {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{ReadyTimeout: "10ms"}},
		"too many mirrors":  {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: strings.Fields(strings.Repeat("http://m ", 9))}},
		"negative timeout":  {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{ReadyTimeout: "-1s"}},
		"runtime case":      {Runtime: "Sysbox-runc", Helper: helper},
		"dind with no flag": {Runtime: "", DinD: &DinDConfig{TestcontainersRyukDisabled: true}},
	} {
		if _, _, err := attemptRuntime(&c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Sysbox defaults fit a nested engine; configured values still apply.
	workloads, err := attemptWorkloads(&OpenCodeConfig{Runtime: "sysbox-runc", Helper: helper, Resources: &docker.ResourceSpec{CPUs: 4}}, &docker.Backend{})
	if err != nil || workloads.Runtime != docker.Sysbox || workloads.Resources.Pids != 2048 || workloads.Resources.NoFile != 65536 || workloads.Resources.NanoCPUs != 4e9 {
		t.Fatal(workloads, err)
	}
}
