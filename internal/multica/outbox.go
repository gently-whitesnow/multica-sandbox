package multica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrDeferred means a terminal callback is durably queued and will be replayed.
var ErrDeferred = errors.New("terminal report queued for replay")

// Terminal is one complete/fail callback. It never carries credentials.
type Terminal struct {
	Task       string `json:"task_id"`
	Failed     bool   `json:"failed,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"failure_reason,omitempty"`
	Session    string `json:"session_id,omitempty"`
	Disposable bool   `json:"session_rollout_missing,omitempty"`
}

// Same replay policy as the upstream daemon terminal report queue.
const (
	replayInitialBackoff = 5 * time.Second
	replayMaxBackoff     = 5 * time.Minute
	rejectionLimit       = 3
	rejectionAge         = 10 * time.Minute
)

// Outbox persists terminal callbacks before delivery and replays them until Multica acknowledges.
type Outbox struct {
	dir    string
	mu     sync.Mutex
	flight map[string]bool
	now    func() time.Time
}

type pending struct {
	Version       int        `json:"version"`
	Terminal      Terminal   `json:"terminal"`
	Attempts      int        `json:"attempts,omitempty"`
	NextAt        time.Time  `json:"next_at"`
	Rejections    int        `json:"rejections,omitempty"`
	FirstRejected *time.Time `json:"first_rejected_at,omitempty"`
}

func (c *Client) UseOutbox(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "failed"), 0700); err != nil {
		return err
	}
	c.outbox = &Outbox{dir: dir, flight: map[string]bool{}, now: time.Now}
	return nil
}

// Deliver sends a terminal callback with the upstream retry schedule. Once the
// callback is persisted, an undelivered report returns ErrDeferred.
func (c *Client) Deliver(ctx context.Context, t Terminal) error {
	if !validID(t.Task) {
		return fmt.Errorf("invalid task ID")
	}
	send := func() error { return retry(ctx, terminalRetry, func() error { return c.sendTerminal(ctx, t) }) }
	o := c.outbox
	if o == nil {
		return send()
	}
	if !o.begin(t.Task) {
		return fmt.Errorf("terminal report already in delivery")
	}
	defer o.end(t.Task)
	// Durability improves availability; a full disk must not block an online callback.
	record := pending{Version: 1, Terminal: t, NextAt: o.now()}
	persisted := o.save(record) == nil
	err := send()
	if !persisted {
		return err
	}
	if err == nil {
		return o.remove(t.Task)
	}
	if ctx.Err() == nil {
		o.settle(record, err)
	}
	return ErrDeferred
}

// Replay retries due queued callbacks once each; it runs before workspace recovery.
func (c *Client) Replay(ctx context.Context) (int, error) {
	o := c.outbox
	if o == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return 0, err
	}
	delivered := 0
	var errs error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var record pending
		data, err := os.ReadFile(filepath.Join(o.dir, entry.Name()))
		if err == nil && (json.Unmarshal(data, &record) != nil || record.Version != 1 || entry.Name() != fileName(record.Terminal.Task)) {
			err = fmt.Errorf("invalid queued terminal report %s", entry.Name())
		}
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if record.NextAt.After(o.now()) || !o.begin(record.Terminal.Task) {
			continue
		}
		err = c.sendTerminal(ctx, record.Terminal)
		if err == nil {
			err = o.remove(record.Terminal.Task)
			if err == nil {
				delivered++
			}
		} else if ctx.Err() == nil {
			o.settle(record, err)
			err = nil
		}
		o.end(record.Terminal.Task)
		errs = errors.Join(errs, err)
	}
	return delivered, errs
}

func (c *Client) sendTerminal(ctx context.Context, t Terminal) error {
	body := map[string]any{"output": t.Output}
	action := "complete"
	if t.Failed {
		action = "fail"
		body = map[string]any{"error": t.Error}
		if t.Reason != "" {
			body["failure_reason"] = t.Reason
		}
	}
	if t.Session != "" {
		body["session_id"] = t.Session
	}
	if t.Disposable {
		body["session_rollout_missing"] = true
	}
	return c.taskPost(ctx, t.Task, action, body)
}

// settle schedules the next replay; only repeated explicit rejections over time quarantine a report.
func (o *Outbox) settle(record pending, cause error) {
	now := o.now()
	record.Attempts++
	backoff := replayInitialBackoff << min(record.Attempts-1, 6)
	record.NextAt = now.Add(min(backoff, replayMaxBackoff))
	var status *HTTPError
	if !Transient(cause) && errors.As(cause, &status) && status.Status >= http.StatusBadRequest {
		record.Rejections++
		if record.FirstRejected == nil {
			record.FirstRejected = &now
		}
		if record.Rejections >= rejectionLimit && now.Sub(*record.FirstRejected) >= rejectionAge {
			if o.save(record) == nil {
				_ = os.Rename(o.path(record.Terminal.Task), filepath.Join(o.dir, "failed", fileName(record.Terminal.Task)))
			}
			return
		}
	}
	_ = o.save(record)
}

func (o *Outbox) begin(task string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.flight[task] {
		return false
	}
	o.flight[task] = true
	return true
}

func (o *Outbox) end(task string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.flight, task)
}

func (o *Outbox) save(record pending) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(o.dir, ".report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), o.path(record.Terminal.Task))
}

func (o *Outbox) remove(task string) error {
	if err := os.Remove(o.path(task)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (o *Outbox) path(task string) string { return filepath.Join(o.dir, fileName(task)) }

// Hashed names keep a malformed task ID from becoming a path.
func fileName(task string) string {
	sum := sha256.Sum256([]byte(task))
	return hex.EncodeToString(sum[:]) + ".json"
}
