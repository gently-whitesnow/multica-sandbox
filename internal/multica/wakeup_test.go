package multica

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWakeupsDeliverDaemonHints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/multica/api/daemon/ws" || r.URL.Query().Get("runtime_ids") != "rt-a,rt-b" || r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := context.Background()
		for _, frame := range []string{
			`{"type":"daemon:pending_work","payload":{"runtime_id":"rt-b","kind":"model_list"}}`,
			`{"type":"daemon:heartbeat_ack","payload":{}}`,
			`not json`,
			`{"type":"daemon:task_available","payload":{"runtime_id":"rt-a","task_id":"t"}}`,
			`{"type":"daemon:workspaces_changed","payload":{}}`,
		} {
			if conn.Write(ctx, websocket.MessageText, []byte(frame)) != nil {
				return
			}
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()
	c, err := New(server.URL+"/multica", "token")
	if err != nil {
		t.Fatal(err)
	}
	connected := false
	var got []Wakeup
	err = c.Wakeups(context.Background(), []string{"rt-b", "rt-a"}, func() { connected = true }, func(w Wakeup) { got = append(got, w) })
	want := []Wakeup{{WakeupPendingWork, "rt-b"}, {WakeupTask, "rt-a"}, {WakeupWorkspaces, ""}}
	if err == nil || !connected || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("wakeups = %v %v %v", err, connected, got)
	}
	var status *HTTPError
	if err = c.Wakeups(context.Background(), []string{"rt-x"}, func() { t.Fatal("connected without a handshake") }, nil); !errors.As(err, &status) || status.Status != 401 {
		t.Fatalf("refused handshake = %v", err)
	}
}

func TestWakeupsEndSilentConnections(t *testing.T) {
	old := wakeupPongWait
	wakeupPongWait = 200 * time.Millisecond
	defer func() { wakeupPongWait = old }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wakeups(context.Background(), []string{"rt"}, func() {}, func(Wakeup) {}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent connection ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silent connection kept open")
	}
}
