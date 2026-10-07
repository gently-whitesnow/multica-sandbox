package opencode

import (
	"strings"
	"testing"
)

func TestCheckImage(t *testing.T) {
	for _, version := range Supported {
		if err := CheckImage([]byte("version "+version+"\n"), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckImage([]byte("missing multica\nversion 1.18.35\n"), false); err != nil {
		t.Fatalf("CLI is optional without the Multica relay: %v", err)
	}
	for name, test := range map[string]struct{ output, want string }{
		"unsupported": {"version 1.18.33\n", `OpenCode "1.18.33" is not verified (supported: 1.18.34, 1.18.35)`},
		"major":       {"version 2.0.24\n", `"2.0.24" is not verified`},
		"missing":     {"", `OpenCode "" is not verified`},
		"suffix":      {"version 1.18.35-dirty\n", `"1.18.35-dirty" is not verified`},
		"managed":     {"override /etc/opencode\nversion 1.18.35\n", `"/etc/opencode" overrides the projected OpenCode configuration`},
		"root config": {"override /.opencode\nversion 1.18.35\n", `"/.opencode" overrides`},
		"env":         {"env OPENCODE_CONFIG_CONTENT\nversion 1.18.35\n", `image ENV sets reserved "OPENCODE_CONFIG_CONTENT"`},
		"cli":         {"missing multica\nversion 1.18.35\n", "multica CLI is not on PATH"},
		"control":     {"version \x1b[31m1.18.35\n", `"\x1b[31m1.18.35" is not verified`},
		"bounded":     {strings.Repeat("env OPENCODE_X\n", 100) + "version 1.18.35\n", "; ..."},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckImage([]byte(test.output), true)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.ContainsRune(err.Error(), '\x1b') || len(err.Error()) > 2048 {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}
