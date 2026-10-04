package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type identity struct{ Server, Workspace, Daemon, Engine string }

// bindState runs under the shared instance lock, before recovery or task claims.
func bindState(dir string, want identity) error {
	path := filepath.Join(dir, "identity.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var existing identity
		if json.Unmarshal(data, &existing) != nil || existing != want {
			return fmt.Errorf("state belongs to a different controller scope or Docker engine")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	data, err = json.Marshal(want)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
