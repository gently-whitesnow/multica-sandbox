package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentIdentity(t *testing.T) {
	dir := t.TempDir()
	original := identity{"https://example.invalid", "daemon", "engine"}
	if err := bindState(dir, original); err != nil {
		t.Fatal(err)
	}
	if err := bindState(dir, original); err != nil {
		t.Fatal(err)
	}
	for _, change := range []identity{
		{"https://other.invalid", "daemon", "engine"},
		{original.Server, "other", "engine"},
		{original.Server, "daemon", "other"},
	} {
		if err := bindState(dir, change); err == nil {
			t.Fatal("changed identity accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := bindState(dir, original); err == nil {
		t.Fatal("corrupt state accepted")
	}
}
func TestConfigRejectsUnknownAndInvalidFields(t *testing.T) {
	valid := `{"server":"https://example.invalid","daemon":"10000000-0000-4000-8000-000000000002","image":"test","command":["/bin/true"],"timeout":"1s"}`
	path := filepath.Join(t.TempDir(), "config.json")
	for _, content := range []string{`{}`, valid + ` {}`, valid[:len(valid)-1] + `,"token":"secret"}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(path); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	valid := `{"server":"https://example.invalid","daemon":"10000000-0000-4000-8000-000000000002","image":"test","command":["/bin/true"],"timeout":"1s"}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadConfig(path)
	if err != nil || c.Concurrency != 1 {
		t.Fatalf("default fleet capacity: %+v %v", c, err)
	}
	for _, extra := range []string{`,"workspace":"10000000-0000-4000-8000-000000000001"}`, `,"workspaces":"all-accessible"}`, `,"concurrency":33}`, `,"concurrency":-1}`} {
		if err := os.WriteFile(path, []byte(valid[:len(valid)-1]+extra), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(path); err == nil {
			t.Fatal("invalid scope/capacity accepted")
		}
	}
}

func TestStateRejectsUnknownFieldsAndTrailingDocuments(t *testing.T) {
	dir := t.TempDir()
	want := identity{"https://example.invalid", "daemon", "engine"}
	for _, data := range []string{`{"Server":"https://example.invalid","Daemon":"daemon","Engine":"engine","Workspace":"*"}`, `{"Server":"https://example.invalid","Daemon":"daemon","Engine":"engine"} {}`} {
		if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := bindState(dir, want); err == nil {
			t.Fatal("unknown identity format accepted")
		}
	}
}
