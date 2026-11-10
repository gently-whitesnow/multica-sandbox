package opencode

import "testing"

func TestSteeringImageGate(t *testing.T) {
	for _, check := range []struct {
		report  string
		allowed bool
	}{
		{"version opencode v2.2.5\nwget /bin/wget\nsync /bin/sync\n", true},
		{"version opencode v2.2.5\nwget /bin/wget\n", false},
		{"version opencode v2.2.5\nwget \n", false},
		{"version opencode v2.2.2\nwget /bin/wget\n", false},
		{"version 1.18.35\nwget /bin/wget\n", false},
		{"version opencode v2.2.7\nwget /bin/wget\n", false},
	} {
		if got := CheckSteeringImage([]byte(check.report)); (got == nil) != check.allowed {
			t.Errorf("CheckSteeringImage(%q) = %v", check.report, got)
		}
	}
}
