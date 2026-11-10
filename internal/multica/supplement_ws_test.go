package multica

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
)

func TestSupplementWebSocketWakesOnlySubscribedNegotiatedTask(t *testing.T) {
	f, _, _ := newSupplementFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, ids := range [][2]string{{"foreign-runtime", f.task.ID}, {f.task.RuntimeID, "foreign-task"}, {f.task.RuntimeID, f.task.ID}, {f.task.RuntimeID, f.task.ID}} {
			data, _ := json.Marshal(map[string]any{"type": WakeupSupplement, "payload": map[string]string{"runtime_id": ids[0], "task_id": ids[1]}})
			if conn.Write(context.Background(), websocket.MessageText, data) != nil {
				return
			}
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()
	c, err := New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UseSupplements(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	c.supplements.negotiate(f.task, true)
	hints := c.SupplementUpdates(f.task)
	err = c.Wakeups(context.Background(), []string{f.task.RuntimeID}, func() {}, func(Wakeup) { t.Fatal("supplement hint started unrelated fleet work") })
	if err == nil {
		t.Fatal("closed socket returned success")
	}
	select {
	case <-hints:
	default:
		t.Fatal("matching native hint did not reach negotiated attempt")
	}
	select {
	case <-hints:
		t.Fatal("duplicate hint was not coalesced")
	default:
	}
}
