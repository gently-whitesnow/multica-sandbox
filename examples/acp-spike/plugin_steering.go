package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

const steeringPlugin = `export default {
  id: "sandbox-steering-fixture",
  setup(ctx) {
    const server = Bun.serve({hostname: "127.0.0.1", port: 4097,
      async fetch(req) {
        if (req.headers.get("authorization") !== "Bearer fixture-steering") return new Response(null, {status: 403});
        try {
          const input = await req.json();
          const result = await ctx.session.prompt({...input, delivery: "steer", resume: false});
          return Response.json(result);
        } catch (error) { return Response.json({error: String(error)}, {status: 500}); }
      }
    });
    return () => server.stop(true);
  }
};`

func (s *spike) projectSteeringPlugin(ctx context.Context) error {
	if !s.v2 {
		return fmt.Errorf("plugin steering requires the v2 plugin SDK")
	}
	if _, err := s.exec(ctx, nil, "mkdir", "-p", "/workspace/steering-plugin"); err != nil {
		return err
	}
	if err := s.write(ctx, "/workspace/steering-plugin/index.js", []byte(steeringPlugin)); err != nil {
		return err
	}
	if err := s.write(ctx, "/workspace/steering-plugin/package.json", []byte(`{"name":"sandbox-steering-fixture","type":"module","main":"index.js"}`)); err != nil {
		return err
	}
	config, err := s.exec(ctx, nil, "cat", "/workspace/opencode.json")
	if err != nil {
		return err
	}
	var projected map[string]any
	if err := json.Unmarshal(config, &projected); err != nil {
		return err
	}
	projected["plugins"] = []string{"file:///workspace/steering-plugin"}
	config, _ = json.Marshal(projected)
	return s.write(ctx, "/workspace/opencode.json", config)
}

func (s *spike) pluginSteeringTurn(ctx context.Context, a *agent, id acp.SessionId) error {
	original := make(chan answerProbe, 1)
	go func() {
		response, err := a.conn.Prompt(ctx, acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock(markPluginSteer)}})
		original <- answerProbe{response, err}
	}()
	if !a.client.waitStatus(ctx, "in_progress", 30*time.Second) {
		return fmt.Errorf("plugin steering: original tool never started")
	}
	body, _ := json.Marshal(map[string]any{"sessionID": id, "id": "msg_steeringfixture", "text": steeringSentinel})
	accepted, err := s.exec(ctx, nil, "wget", "-q", "-O", "-", "--header=Authorization: Bearer fixture-steering", "--header=Content-Type: application/json", "--post-data="+string(body), "http://127.0.0.1:4097/")
	if err != nil {
		return fmt.Errorf("plugin steering admission: %w; %s", err, accepted)
	}
	select {
	case result := <-original:
		return fmt.Errorf("original turn ended before release: %s, %v", result.response.StopReason, result.err)
	default:
	}
	if _, err := s.exec(ctx, nil, "touch", "/workspace/steering-release"); err != nil {
		return err
	}
	select {
	case result := <-original:
		events := a.client.drain()
		if result.err != nil || result.response.StopReason != acp.StopReasonEndTurn || !steeringObserved.Load() || !strings.Contains(text(events), "STEERING-CONSUMED-IN-ORIGINAL-TURN") {
			return fmt.Errorf("plugin steering failed: reason=%s error=%v inference=%v transcript=%s", result.response.StopReason, result.err, steeringObserved.Load(), text(events))
		}
		s.check("same ACP turn steered", true, "native plugin admission=%s; original end_turn; sentinel seen by inference and ACP transcript", accepted)
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("plugin steering: original turn never completed")
	}
}

type answerProbe struct {
	response acp.PromptResponse
	err      error
}
