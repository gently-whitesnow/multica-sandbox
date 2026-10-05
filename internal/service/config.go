package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
)

type Config struct {
	OpenCode    *OpenCodeConfig `json:"opencode,omitempty"`
	Server      string          `json:"server"`
	Concurrency int             `json:"concurrency,omitempty"`
	Daemon      string          `json:"daemon"`
	Image       string          `json:"image"`
	Command     []string        `json:"command"`
	Timeout     string          `json:"timeout"`
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
	if err != nil || duration <= 0 || !uuid.MatchString(c.Daemon) || c.Concurrency < 0 || c.Concurrency > 32 || c.Image == "" || (len(c.Command) == 0 && c.OpenCode == nil) {
		return c, fmt.Errorf("daemon UUID, capacity 0-32, image, command and positive timeout required")
	}
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	return c, nil
}

type OpenCodeConfig struct {
	IdentityFile string         `json:"identity_file"`
	Authority    attempt.Config `json:"authority"`
	Network      string         `json:"network"`
	Peers        []string       `json:"peers"`
}
