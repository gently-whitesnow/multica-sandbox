//go:build linux || darwin

package instance

import (
	"fmt"
	"os"
	"syscall"
)

// Lock requires all processes for one daemon identity to share a trusted lock path.
func Lock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open controller lock: %w", err)
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("controller identity already in use or lock unavailable")
	}
	return func() { file.Close() }, nil
}
