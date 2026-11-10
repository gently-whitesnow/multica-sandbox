package opencode

import (
	"context"
	"errors"
	"strings"
	"time"
)

const httpWithdrawGrace = 2 * time.Second

type httpInboxItem struct {
	ID      string `json:"id"`
	Payload struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"payload"`
}

// deliver only admits to the existing execution. Stable IDs make retries of an
// ambiguous HTTP response idempotent in the native inbox and canonical history.
func (h *httpTurn) deliver(ctx context.Context, id, text string) error {
	if !strings.HasPrefix(id, "msg_") || len(id) > 160 || strings.ContainsAny(id, "/?#\x00\r\n") {
		return ErrDenied
	}
	_, busy, _, generation, _, _, _ := h.status()
	h.mu.Lock()
	prior, known := h.delivered[id]
	h.mu.Unlock()
	if known && h.consumed(ctx, id, prior.execution) {
		return nil
	}
	if !busy {
		return errHTTPInputEnded
	}
	path := "/api/session/" + h.session
	body := map[string]any{"id": id, "text": text, "delivery": "steer", "resume": false, "metadata": h.r.httpInputMetadata("supplement")}
	admissionErr := h.r.httpAPI(ctx, "POST", path+"/prompt", body, nil)
	retryAfter := time.Now().Add(supplementPollInterval)
	ticker := time.NewTicker(httpPollInterval)
	defer ticker.Stop()
	for {
		if h.consumed(ctx, id, generation) {
			return nil
		}
		_, active, terminal, current, _, _, _ := h.status()
		if terminal || !active || current != generation || ctx.Err() != nil {
			return h.withdraw(ctx, id, generation)
		}
		if admissionErr != nil && time.Now().After(retryAfter) {
			if h.r.adapter.Log != nil {
				h.r.adapter.Log.Info("additional input admission unanswered; resending under the same id", "task", h.r.task.ID, "attempt", h.r.task.AttemptKey(), "input", id)
			}
			admissionErr = h.r.httpAPI(ctx, "POST", path+"/prompt", body, nil)
			retryAfter = time.Now().Add(supplementPollInterval)
		}
		select {
		case <-ctx.Done():
			return h.withdraw(ctx, id, generation)
		case <-ticker.C:
		case <-h.changed:
		}
	}
}

func (h *httpTurn) consumed(ctx context.Context, id string, generation int64) bool {
	h.mu.Lock()
	promotion, promoted := h.delivered[id]
	if !promoted || !promotion.processed || promotion.execution != generation {
		h.mu.Unlock()
		return false
	}
	if promotion.verified || time.Now().Before(promotion.verifyAfter) {
		h.mu.Unlock()
		return promotion.verified
	}
	promotion.verifyAfter = time.Now().Add(time.Second)
	h.delivered[id] = promotion
	h.mu.Unlock()
	var message struct {
		Data struct {
			ID, Type string
			Time     struct{ Created int64 }
		} `json:"data"`
	}
	read, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := h.r.httpAPI(read, "GET", "/api/session/"+h.session+"/message/"+id, nil, &message); err != nil {
		return false
	}
	// Promotion alone is not enough: a step must have dispatched after the
	// canonical input was promoted, within the execution that accepted it.
	if message.Data.ID != id || message.Data.Type != "user" || message.Data.Time.Created <= 0 {
		return false
	}
	h.mu.Lock()
	promotion.verified = true
	h.delivered[id] = promotion
	h.mu.Unlock()
	return true
}

func (h *httpTurn) withdraw(ctx context.Context, id string, generation int64) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpWithdrawGrace)
	defer cancel()
	if h.consumed(cleanup, id, generation) {
		return nil
	}
	// Cancellation only withdraws pending input. It never interrupts the agent
	// loop, and a concurrent promotion wins atomically in OpenCode's inbox.
	err := h.r.httpAPI(cleanup, "DELETE", "/api/session/"+h.session+"/inbox/"+id, nil, nil)
	ticker := time.NewTicker(httpPollInterval)
	defer ticker.Stop()
	for {
		if h.consumed(cleanup, id, generation) {
			return nil
		}
		h.mu.Lock()
		cancelled := h.cancelled[id]
		h.mu.Unlock()
		if cancelled {
			return errHTTPInputEnded
		}
		select {
		case <-cleanup.Done():
			return h.uncertainWithdrawal(err)
		case <-ticker.C:
		case <-h.changed:
		}
	}
}

func (h *httpTurn) uncertainWithdrawal(err error) error {
	h.mu.Lock()
	h.unsafeResume = true
	h.r.nativeClean = false
	h.mu.Unlock()
	if err != nil {
		return errors.Join(errHTTPInputEnded, err)
	}
	return errHTTPInputEnded
}

func (r *running) httpStaleInput(ctx context.Context, session string) error {
	var inbox struct {
		Data []httpInboxItem `json:"data"`
	}
	if err := r.httpAPI(ctx, "GET", "/api/session/"+session+"/inbox", nil, &inbox); err != nil {
		return err
	}
	for _, item := range inbox.Data {
		attempt := item.Payload.Metadata["multica_attempt"]
		if attempt == "" || attempt == r.task.AttemptKey() {
			continue
		}
		if !strings.HasPrefix(item.ID, "msg_") || strings.ContainsAny(item.ID, "/?#\x00\r\n") {
			return ErrDenied
		}
		if err := r.httpAPI(ctx, "DELETE", "/api/session/"+session+"/inbox/"+item.ID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
