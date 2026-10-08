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
)

const pinnedCLI = "cli-version multica dev (commit: b4ca5b4a23e68b26292a680dca7689a952bb1cd5, built: unknown)\n"
const controllerCLI = "cli /opt/multica-sandbox/multica/bin/multica\n" + pinnedCLI

func TestCheckImage(t *testing.T) {
	for _, version := range Supported {
		if err := CheckImage([]byte("version "+version+"\n"+controllerCLI), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckImage([]byte("cli /usr/local/bin/multica\ncli-version multica 0.1\nversion 1.18.35\n"), false); err != nil {
		t.Fatalf("CLI is ignored without the Multica relay: %v", err)
	}
	for name, test := range map[string]struct{ output, want string }{
		"unsupported": {"version 1.18.33\n", `OpenCode "1.18.33" is not verified (supported: 1.18.34, 1.18.35)`},
		"major":       {"version 2.0.24\n", `"2.0.24" is not verified`},
		"missing":     {"", `OpenCode "" is not verified`},
		"suffix":      {"version 1.18.35-dirty\n", `"1.18.35-dirty" is not verified`},
		"managed":     {"override /etc/opencode\nversion 1.18.35\n", `"/etc/opencode" overrides the projected OpenCode configuration`},
		"root config": {"override /.opencode\nversion 1.18.35\n", `"/.opencode" overrides`},
		"env":         {"env OPENCODE_CONFIG_CONTENT\nversion 1.18.35\n", `image ENV sets reserved "OPENCODE_CONFIG_CONTENT"`},
		"cli":         {"cli \ncli-version \nversion 1.18.35\n", `multica resolves to "", not the controller artifact`},
		"shadowed":    {"cli /usr/local/bin/multica\n" + pinnedCLI + "version 1.18.35\n", `"/usr/local/bin/multica", not the controller artifact`},
		"revision":    {"cli /opt/multica-sandbox/multica/bin/multica\ncli-version multica dev (commit: unknown, built: unknown)\nversion 1.18.35\n", "is not built from Multica b4ca5b4a23e68b26292a680dca7689a952bb1cd5"},
		"control":     {"version \x1b[31m1.18.35\n" + controllerCLI, `"\x1b[31m1.18.35" is not verified`},
		"bounded":     {strings.Repeat("env OPENCODE_X\n", 100) + "version 1.18.35\n" + controllerCLI, "; ..."},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckImage([]byte(test.output), true)
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
