package multica

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Wakeup is a hint from the upstream daemon WebSocket that work is queued for Runtime
// (empty: any runtime). Hints carry no work and may be lost or repeated; the heartbeat,
// claim or workspace sync they trigger fetches the work, as in the upstream daemon.
type Wakeup struct{ Event, Runtime string }

const (
	WakeupPendingWork = "daemon:pending_work"
	WakeupTask        = "daemon:task_available"
	WakeupWorkspaces  = "daemon:workspaces_changed"
)

// wakeupPongWait ends a connection without frames or server pings for this long; the
// upstream hub pings every 54 s and drops silent clients after 60 s.
var wakeupPongWait = 60 * time.Second

// Wakeups holds the daemon WebSocket for runtimes and delivers its hints until ctx ends
// or the connection fails; connected runs once the handshake succeeds.
func (c *Client) Wakeups(ctx context.Context, runtimes []string, connected func(), deliver func(Wakeup)) error {
	u, err := url.Parse(c.base)
	if err != nil {
		return errTransport
	}
	u.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	u.Path = strings.TrimRight(u.Path, "/") + "/api/daemon/ws"
	u.RawQuery = url.Values{"runtime_ids": {strings.Join(slices.Sorted(slices.Values(runtimes)), ",")}}.Encode()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wait := wakeupPongWait
	alive := make(chan struct{}, 1)
	signal := func() {
		select {
		case alive <- struct{}{}:
		default:
		}
	}
	handshake, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()
	conn, resp, err := websocket.Dial(handshake, u.String(), &websocket.DialOptions{
		// Same transport and redirect policy as the API client.
		HTTPClient:     &http.Client{Transport: c.http.Transport, CheckRedirect: c.http.CheckRedirect},
		HTTPHeader:     http.Header{"Authorization": {"Bearer " + c.token}},
		OnPingReceived: func(context.Context, []byte) bool { signal(); return true },
	})
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		return &HTTPError{Status: resp.StatusCode}
	}
	if err != nil {
		return errTransport
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	connected()
	go func() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-alive:
				timer.Reset(wait)
			case <-timer.C:
				cancel()
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return errTransport
		}
		signal()
		var msg struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		var payload struct {
			Runtime string `json:"runtime_id"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		_ = json.Unmarshal(msg.Payload, &payload)
		switch msg.Type {
		case WakeupPendingWork, WakeupTask, WakeupWorkspaces:
			deliver(Wakeup{msg.Type, payload.Runtime})
		}
	}
}
