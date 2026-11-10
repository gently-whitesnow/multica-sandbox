package opencode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

const nativeHTTPOrigin = "http://127.0.0.1:4096"

var errHTTPInputEnded = errors.New("OpenCode active turn ended before additional input was consumed")

func (r *running) nativeHTTPEnvironment(env map[string]string) {
	if r.nativePassword == "" {
		return
	}
	env["OPENCODE_PASSWORD"] = r.nativePassword
	env["OPENCODE_CONFIG"] = "/workspace/opencode.json"
	env["SANDBOX_HTTP_AUTH"] = base64.StdEncoding.EncodeToString([]byte("opencode:" + r.nativePassword))
}

// configureNativeHTTP keeps native session-owned MCP selection equivalent to ACP.
func (r *running) configureNativeHTTP(config []byte) ([]byte, error) {
	if r.adapter.Steering == nil {
		return config, nil
	}
	var projected map[string]any
	if err := json.Unmarshal(config, &projected); err != nil {
		return nil, err
	}
	connections := map[string]any{}
	for _, server := range r.sessionMCP() {
		if server.Http == nil {
			return nil, fmt.Errorf("HTTP transport requires HTTP MCP")
		}
		headers := map[string]string{}
		for _, header := range server.Http.Headers {
			headers[header.Name] = header.Value
		}
		connections[server.Http.Name] = map[string]any{"type": "remote", "url": server.Http.Url, "headers": headers, "oauth": false}
	}
	projected["mcp"] = map[string]any{"servers": connections}
	return json.Marshal(projected)
}

// httpAPI uses the maintained CLI against this attempt's already-running owner.
func (r *running) httpAPI(ctx context.Context, method, path string, body any, out any) error {
	args := []string{"opencode", "--server", nativeHTTPOrigin, "api", method, path}
	var request []byte
	if body != nil {
		if method != "POST" {
			return ErrDenied
		}
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		request = data
		if len(request) > 4<<20 {
			return ErrDenied
		}
		args = []string{"/bin/sh", "-c", `set -eu; umask 077; f=$2; trap 'rm -f "$f"' EXIT HUP INT TERM; cat > "$f"; wget -q -O - --header "Authorization: Basic $SANDBOX_HTTP_AUTH" --header 'Content-Type: application/json' --post-file "$f" "$1"`, "native-http", nativeHTTPOrigin + path, "/workspace/native-http-" + rand.Text() + ".json"}
	}
	var data []byte
	err := r.stream(ctx, args, &r.stderr, func(stdin io.Writer, stdout io.Reader) error {
		if request != nil {
			if _, err := stdin.Write(request); err != nil {
				return err
			}
			closer, ok := stdin.(io.Closer)
			if !ok {
				return &execution.HostFailure{Message: "Sandbox native HTTP input cannot be closed"}
			}
			if err := closer.Close(); err != nil {
				return err
			}
		}
		var err error
		data, err = io.ReadAll(io.LimitReader(stdout, (16<<20)+1))
		if len(data) > 16<<20 {
			return &execution.AgentFailure{Message: "OpenCode native HTTP response exceeded its bound"}
		}
		return err
	})
	if err != nil {
		var oom *execution.OOMError
		var stall *execution.MemoryStallError
		if errors.As(err, &oom) || errors.As(err, &stall) {
			return err
		}
		return &execution.AgentFailure{Message: "OpenCode native HTTP request failed"}
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return &execution.AgentFailure{Message: "OpenCode native HTTP response invalid"}
	}
	return nil
}

type nativeSession struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	ParentID string `json:"parentID"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Time struct{ Busy, Idle int64 } `json:"time"`
}

func (r *running) httpSession(ctx context.Context) (string, error) {
	var response struct {
		Data nativeSession `json:"data"`
	}
	if r.resume != "" {
		err := r.httpAPI(ctx, "GET", "/api/session/"+r.resume, nil, &response)
		if err == nil && response.Data.ID == r.resume && response.Data.Location.Directory == repo.WorkDir {
			if err := r.httpStaleInput(ctx, r.resume); err != nil {
				return "", err
			}
			return r.httpModel(ctx, response.Data.ID)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		prompt, _, err := Prompt(r.task, r.adapter.Relay != nil, r.adapter.Checkout != nil, Lost)
		if err != nil {
			return "", err
		}
		r.prompt, r.retired, r.resume = prompt, r.resume, ""
	}
	body := map[string]any{"location": map[string]string{"directory": repo.WorkDir}}
	if err := r.httpAPI(ctx, "POST", "/api/session", body, &response); err != nil {
		return "", err
	}
	if !sessionID.MatchString(response.Data.ID) || response.Data.Location.Directory != repo.WorkDir {
		return "", &execution.AgentFailure{Message: "OpenCode returned an invalid HTTP session"}
	}
	return r.httpModel(ctx, response.Data.ID)
}

func (r *running) httpModel(ctx context.Context, session string) (string, error) {
	if r.model == "" {
		return session, nil
	}
	provider, model, ok := strings.Cut(r.model, "/")
	if !ok || provider == "" || model == "" {
		return "", ErrDenied
	}
	selection := map[string]string{"providerID": provider, "id": model}
	if r.effort != "" {
		selection["variant"] = r.effort
	}
	err := r.httpAPI(ctx, "POST", "/api/session/"+session+"/model", map[string]any{"model": selection}, nil)
	return session, err
}

func (r *running) httpMessages(ctx context.Context, session string, since int64) ([]nativeMessage, error) {
	var all []nativeMessage
	cursor := ""
	for range followPages {
		query := url.Values{"limit": {fmt.Sprint(followPage)}}
		if cursor == "" {
			query.Set("order", "desc")
		} else {
			query.Set("cursor", cursor)
		}
		var page struct {
			Data   []nativeMessage `json:"data"`
			Cursor struct {
				Next string `json:"next"`
			} `json:"cursor"`
		}
		if err := r.httpAPI(ctx, "GET", "/api/session/"+session+"/message?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, message := range page.Data {
			if message.Time.Created < since {
				return all, nil
			}
			all = append(all, message)
		}
		if len(page.Data) < followPage || page.Cursor.Next == "" {
			return all, nil
		}
		cursor = page.Cursor.Next
	}
	return nil, errFollow
}

func (r *running) httpSpent(ctx context.Context, session string, since time.Time) {
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageTimeout)
	defer cancel()
	messages, err := r.httpMessages(read, session, since.UnixMilli())
	if err == nil {
		r.spent.usage = multicaUsage(messages)
	}
	r.spent.answered = true
	r.events.reportUsage(r.spent.usage)
}

func multicaUsage(messages []nativeMessage) (usage multica.Usage) {
	for _, message := range messages {
		if message.Type == "assistant" {
			usage = addUsage(usage, message.usage())
		}
	}
	return usage
}

func drainHTTPOutput(ctx context.Context, stdout io.Reader) {
	if closer, ok := stdout.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	_, _ = io.Copy(io.Discard, stdout)
}
