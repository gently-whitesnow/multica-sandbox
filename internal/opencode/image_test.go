package opencode

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const pinnedCLI = "cli-version multica dev (commit: f05a525ac8e82f5e9169ad078e44f166fad6d5c8, built: unknown)\n"
const controllerCLI = "cli /opt/multica-sandbox/multica/bin/multica\n" + pinnedCLI + "tar /bin/tar\n"

func TestCheckImage(t *testing.T) {
	for _, version := range Supported {
		if err := CheckImage([]byte("version "+version+"\n"+controllerCLI+"git /usr/bin/git\n"), true, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckImage([]byte("version opencode v2.2.2\n"), false, false); err != nil {
		t.Fatalf("2.x version line rejected: %v", err)
	}
	if err := CheckImage([]byte("cli /usr/local/bin/multica\ncli-version multica 0.1\nversion 1.18.35\ngit \n"), false, false); err != nil {
		t.Fatalf("CLI and git are ignored without the relays: %v", err)
	}
	if err := CheckImage([]byte("version 1.18.35\n"+controllerCLI+"git \n"), true, true); err == nil || !strings.Contains(err.Error(), "git is not installed") {
		t.Fatalf("missing git accepted for repository checkout: %v", err)
	}
	if err := CheckImage([]byte("version 1.18.35\n"+strings.Replace(controllerCLI, "tar /bin/tar", "tar ", 1)), true, false); err == nil || !strings.Contains(err.Error(), "tar is not installed") {
		t.Fatalf("missing tar accepted for skills: %v", err)
	}
	for name, test := range map[string]struct{ output, want string }{
		"unsupported": {"version 1.18.33\n", `OpenCode "1.18.33" is not verified (supported: 1.18.34, 1.18.35, 2.2.2)`},
		"v2 prefix":   {"version opencode v2.2.3\n", `OpenCode "2.2.3" is not verified`},
		"v2 garbage":  {"version opencode v2.2.2 extra\n", `"opencode v2.2.2 extra" is not verified`},
		"major":       {"version 2.0.24\n", `"2.0.24" is not verified`},
		"missing":     {"", `OpenCode "" is not verified`},
		"suffix":      {"version 1.18.35-dirty\n", `"1.18.35-dirty" is not verified`},
		"managed":     {"override /etc/opencode\nversion 1.18.35\n", `"/etc/opencode" overrides the projected OpenCode configuration`},
		"root config": {"override /.opencode\nversion 1.18.35\n", `"/.opencode" overrides`},
		"env":         {"env OPENCODE_CONFIG_CONTENT\nversion 1.18.35\n", `image ENV sets reserved "OPENCODE_CONFIG_CONTENT"`},
		"cli":         {"cli \ncli-version \nversion 1.18.35\n", `multica resolves to "", not the controller artifact`},
		"shadowed":    {"cli /usr/local/bin/multica\n" + pinnedCLI + "version 1.18.35\n", `"/usr/local/bin/multica", not the controller artifact`},
		"revision":    {"cli /opt/multica-sandbox/multica/bin/multica\ncli-version multica dev (commit: unknown, built: unknown)\nversion 1.18.35\n", "is not built from Multica f05a525ac8e82f5e9169ad078e44f166fad6d5c8"},
		"control":     {"version \x1b[31m1.18.35\n" + controllerCLI, `"\x1b[31m1.18.35" is not verified`},
		"bounded":     {strings.Repeat("env OPENCODE_X\n", 100) + "version 1.18.35\n" + controllerCLI, "; ..."},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckImage([]byte(test.output), true, false)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.ContainsRune(err.Error(), '\x1b') || len(err.Error()) > 2048 {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

// TestOfficialImagesAreListed keeps every tracked official OpenCode reference on the verified list.
func TestOfficialImagesAreListed(t *testing.T) {
	files, err := exec.Command("git", "-C", "../..", "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	reference := regexp.MustCompile(`ghcr\.io/anomalyco/opencode[:@][^\s"'\x60)]*`)
	found := map[string]bool{}
	for _, name := range bytes.Split(bytes.TrimSuffix(files, []byte{0}), []byte{0}) {
		data, err := os.ReadFile(filepath.Join("../..", string(name)))
		if err != nil {
			t.Fatal(err)
		}
		for _, image := range reference.FindAllString(string(data), -1) {
			found[image] = true
			if !slices.Contains(Images, image) {
				t.Errorf("%s: %s is not in images.txt", name, image)
			}
		}
	}
	if len(found) != len(Images) || len(Images) == 0 {
		t.Fatalf("found %d listed images, want all %d", len(found), len(Images))
	}
}

func TestCheckImageRevision(t *testing.T) {
	fork := strings.Repeat("c", 40)
	cli := "cli " + multica.CLIDir + "/bin/multica\ncli-version multica dev (commit: " + fork + ", built: unknown)\ntar /usr/bin/tar\n"
	if err := CheckImageRevision([]byte("version 1.18.35\n"+cli+"git /usr/bin/git\n"), fork, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckImage([]byte("version 1.18.35\n"+cli+"git /usr/bin/git\n"), true, true); err == nil {
		t.Fatal("CLI from another revision accepted against the default pin")
	}
}

func TestCheckEngineForSysbox(t *testing.T) {
	engine := "engine dockerd /usr/local/bin/dockerd\nengine containerd /usr/local/bin/containerd\nengine runc /usr/local/bin/runc\nengine iptables /usr/sbin/iptables\nengine-version Docker version 29.7.1, build 1234567\n"
	if err := CheckEngine([]byte("version 1.18.35\n" + engine)); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{
		"no dockerd":   strings.Replace(engine, "dockerd /usr/local/bin/dockerd", "dockerd ", 1),
		"no iptables":  strings.Replace(engine, "engine iptables /usr/sbin/iptables\n", "", 1),
		"no version":   strings.Replace(engine, "Docker version", "", 1),
		"relative bin": strings.Replace(engine, "runc /usr/local/bin/runc", "runc runc", 1),
		"empty":        "",
	} {
		if err := CheckEngine([]byte(output)); err == nil || !strings.Contains(err.Error(), "sysbox-runc attempts need") {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
