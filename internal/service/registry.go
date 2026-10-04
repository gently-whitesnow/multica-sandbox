package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func readRegistry(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "runtimes.json"))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var known map[string]string
	if json.Unmarshal(data, &known) != nil || known == nil {
		return nil, fmt.Errorf("invalid runtime registry")
	}
	for ws, rt := range known {
		if !uuid.MatchString(ws) || !uuid.MatchString(rt) {
			return nil, fmt.Errorf("invalid runtime registry scope")
		}
	}
	return known, nil
}

func writeRegistry(dir string, known map[string]string) error {
	data, err := json.Marshal(known)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".runtimes-*")
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
	return os.Rename(f.Name(), filepath.Join(dir, "runtimes.json"))
}
