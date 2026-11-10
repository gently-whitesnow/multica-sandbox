package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type probeHTTPAPI func(string, any, ...string) ([]byte, error)

func (s *spike) startProbeHTTP(ctx context.Context) (probeHTTPAPI, error) {
	if !s.v2 {
		return nil, fmt.Errorf("HTTP steering probe currently requires the v2 API")
	}
	_, err := docker(ctx, nil, "exec", "-d", "--workdir", workDir, "--env", "OPENCODE_PASSWORD=fixture-password", "--env", "OPENCODE_CONFIG=/workspace/opencode.json", s.container, "opencode", "serve", "--hostname", "127.0.0.1", "--port", "4096")
	if err != nil {
		return nil, err
	}
	// The native CLI uses the same explicitly owned endpoint for every request.
	api := func(operation string, body any, params ...string) ([]byte, error) {
		args := []string{"/bin/sh", "-c", `export OPENCODE_PASSWORD=fixture-password; exec "$@"`, "api", "opencode", "--server", "http://127.0.0.1:4096", "api", operation}
		if body != nil {
			data, _ := json.Marshal(body)
			args = append(args, "--data", string(data))
		}
		for _, param := range params {
			args = append(args, "--param", param)
		}
		return s.exec(ctx, nil, args...)
	}
	for i := 0; ; i++ {
		if _, err := api("server.info", nil); err == nil {
			break
		}
		if i == 30 {
			return nil, fmt.Errorf("HTTP service did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if debug {
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:fixture-password"))
		data, err := s.exec(ctx, nil, "timeout", "2", "wget", "-S", "-O", "-", "--header="+header, "http://127.0.0.1:4096/api/event")
		fmt.Printf("raw SSE fixture sample=%s; timeout=%v\n", data, err)
	}
	return api, nil
}

func (s *spike) httpSteeringTurn(ctx context.Context) error {
	api, err := s.startProbeHTTP(ctx)
	if err != nil {
		return err
	}
	created, err := api("session.create", map[string]any{"location": map[string]string{"directory": workDir}})
	if err != nil {
		return err
	}
	var session struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created, &session); err != nil || session.Data.ID == "" {
		return fmt.Errorf("HTTP session creation: %s", created)
	}
	id := session.Data.ID
	done := make(chan error, 1)
	var output []byte
	go func() {
		var err error
		output, err = s.exec(ctx, nil, "/bin/sh", "-c", `export OPENCODE_PASSWORD=fixture-password; exec "$@"`, "run", "opencode", "run", "--server", "http://127.0.0.1:4096", "--session", id, "--format", "json", markPluginSteer)
		done <- err
	}()
	for i := 0; ; i++ {
		messages, err := api("session.message.list", nil, "sessionID="+id)
		if err == nil && strings.Contains(string(messages), "Await steering fixture") {
			break
		}
		if i == 30 {
			return fmt.Errorf("original HTTP tool not active: %s, %v", messages, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	accepted, err := api("session.prompt", map[string]any{"id": "msg_httpsteeringfixture", "text": steeringSentinel, "delivery": "steer", "resume": false}, "sessionID="+id)
	if err != nil {
		return fmt.Errorf("HTTP steering admission: %v: %s", err, accepted)
	}
	select {
	case err := <-done:
		return fmt.Errorf("original CLI ended before admission: %v", err)
	default:
	}
	if _, err := s.exec(ctx, nil, "touch", "/workspace/steering-release"); err != nil {
		return err
	}
	select {
	case err := <-done:
		if err != nil || !steeringObserved.Load() || !strings.Contains(string(output), "STEERING-CONSUMED-IN-ORIGINAL-TURN") {
			return fmt.Errorf("same-service HTTP steering: err=%v inference=%v output=%s", err, steeringObserved.Load(), output)
		}
		s.check("same HTTP run steered", true, "session=%s stable native service; original CLI still active at admission; original exit0 and model consumed guidance", id)
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("original HTTP CLI did not finish")
	}
}
