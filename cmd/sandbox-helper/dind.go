package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Nested engine paths under the attempt's /run tmpfs (ADR 0019); /var/run resolves to
// /run, so clients find the socket without DOCKER_HOST.
const (
	dockerSocket = "/run/docker.sock"
	readyMarker  = "/run/dind.ready"
	engineLog    = "/run/dockerd.log"
)

// dind is PID 1 of a sysbox attempt: it restores the /workspace mode that sysbox-runc
// resets with --workdir, starts dockerd for the attempt group, marks readiness once the
// API answers, reaps orphans and forwards termination. It exits with dockerd.
func dind(flags []string) error {
	args := []string{"--host=unix://" + dockerSocket, "--group=" + strconv.Itoa(attemptUser), "--exec-root=/run/docker", "--pidfile=/run/docker.pid", "--log-level=warn"}
	for _, flag := range flags {
		if !strings.HasPrefix(flag, "--registry-mirror=") && !strings.HasPrefix(flag, "--insecure-registry=") {
			return fmt.Errorf("unsupported dind flag")
		}
		args = append(args, flag)
	}
	if err := os.Chmod("/workspace", fs.ModeSticky|0o777); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	logFile, err := os.OpenFile(engineLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	engine := exec.Command("dockerd", args...)
	log := &cappedFile{f: logFile, room: 1 << 20}
	engine.Stdout, engine.Stderr = log, log
	if err := engine.Start(); err != nil {
		return err
	}
	go markReady()
	for sig := range signals {
		if sig != syscall.SIGCHLD {
			_ = engine.Process.Signal(sig)
			continue
		}
		// engine.Wait is never called: the reaper collects dockerd too.
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if pid == engine.Process.Pid {
				_ = os.Remove(readyMarker)
				os.Exit(status.ExitStatus())
			}
		}
	}
	return nil
}

// markReady writes the marker after the engine API answers /_ping.
func markReady() {
	client := http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket)
	}}}
	for {
		if resp, err := client.Get("http://docker/_ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				_ = os.WriteFile(readyMarker, nil, 0o644)
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dindWait waits for the readiness marker; the controller runs it as the attempt user.
func dindWait(timeout string) error {
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		return fmt.Errorf("invalid timeout")
	}
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(readyMarker); err == nil {
			return nil
		}
	}
	return fmt.Errorf("nested Docker not ready within %s", d)
}

// cappedFile bounds the engine log on the /run tmpfs and drops the rest.
type cappedFile struct {
	f    *os.File
	room int
}

func (c *cappedFile) Write(p []byte) (int, error) {
	if c.room > 0 {
		n, _ := c.f.Write(p[:min(len(p), c.room)])
		c.room -= n
	}
	return len(p), nil
}
