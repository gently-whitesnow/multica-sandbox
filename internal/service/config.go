package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

type Config struct {
	Server    string   `json:"server"`
	Workspace string   `json:"workspace"`
	Daemon    string   `json:"daemon"`
	Image     string   `json:"image"`
	Command   []string `json:"command"`
	Timeout   string   `json:"timeout"`
}

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func ReadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("open controller config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	if info.Size() > 65536 {
		return c, fmt.Errorf("controller configuration exceeds size limit")
	}
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid controller configuration")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return c, fmt.Errorf("unexpected trailing configuration")
	}
	duration, err := time.ParseDuration(c.Timeout)
	if err != nil || duration <= 0 || !uuid.MatchString(c.Workspace) || !uuid.MatchString(c.Daemon) || c.Image == "" || len(c.Command) == 0 {
		return c, fmt.Errorf("valid workspace/daemon UUIDs, image, command and positive timeout required")
	}
	return c, nil
}
