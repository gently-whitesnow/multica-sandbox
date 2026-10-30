package opencode

import (
	"strings"
	"testing"
)

func TestExportUsageRejectsIncompleteHistory(t *testing.T) {
	for _, data := range []string{
		``, `{}`, `{"messages":{}}`, `{"messages":[{"info":{"role":"assistant","time":{"created":2},"tokens":{"input":10}}}`, `{"messages":[]`,
	} {
		if _, err := exportUsage(strings.NewReader(data), 1); err == nil {
			t.Fatalf("incomplete export accepted: %q", data)
		}
	}
}

func TestExportUsageSkipsMetadataAndOldMessages(t *testing.T) {
	data := `banner\n{"info":{"nested":[{"a":true},null]},"messages":[` +
		`{"info":{"role":"assistant","time":{"created":1},"tokens":{"input":100}}},` +
		`{"info":{"role":"user","time":{"created":2},"tokens":{"input":100}}},` +
		`{"info":{"role":"assistant","time":{"created":2},"tokens":{"input":10,"output":-1,"reasoning":2}}}],"metadata":[1,2]}`
	u, err := exportUsage(strings.NewReader(data), 2)
	if err != nil || u.Input != 10 || u.Output != 2 {
		t.Fatalf("usage: %+v, %v", u, err)
	}
}
