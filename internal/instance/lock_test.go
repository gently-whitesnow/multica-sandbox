//go:build linux || darwin

package instance

import (
	"path/filepath"
	"testing"
)

func TestExclusiveOwnershipAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if extra, err := Lock(path); err == nil {
		extra()
		release()
		t.Fatal("second controller admitted")
	}
	release()
	again, err := Lock(path)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	again()
}
