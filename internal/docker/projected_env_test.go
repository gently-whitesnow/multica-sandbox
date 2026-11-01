package docker

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestExecEnvironmentSupportsNativeValuesWithoutCredentialArgv(t *testing.T) {
	r := &projectedRun{run: &run{name: "attempt"}}
	env := map[string]string{"lower": "line-one\nline-two\r\n", "_TOKEN": strings.Repeat("private", 1024)}
	cmd, err := r.exec(context.Background(), []string{"true"}, env)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range env {
		if !slices.Contains(cmd.Env, name+"="+value) || !slices.Contains(cmd.Args, name) {
			t.Fatal("native environment was not delivered by name")
		}
		if strings.Contains(strings.Join(cmd.Args, " "), value) {
			t.Fatal("environment value entered command arguments")
		}
	}
	for _, invalid := range []map[string]string{{"BAD=NAME": "x"}, {"TOKEN": "private\x00value"}, {"1TOKEN": "x"}, {"DOCKER_HOST": "tcp://private.invalid:2375"}} {
		if _, err = r.exec(context.Background(), []string{"true"}, invalid); err == nil {
			t.Fatal("invalid OS environment accepted")
		}
	}
}
