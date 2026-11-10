package multica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
)

// ProcessSupplement advances one negotiated attempt serially. deliver must dedup
// by stableMessageID and return nil only after consumption in this original turn.
// Admission alone is not consumption. Errors retain the durable phase for retry.
func (c *Client) ProcessSupplement(ctx context.Context, t Task, session string, deliver func(context.Context, string, string) error) (bool, error) {
	if !c.SupplementsNegotiated(t) {
		return false, nil
	}
	if !supplementSession.MatchString(session) || deliver == nil {
		return false, fmt.Errorf("invalid supplement delivery session")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s := c.supplements
	key := t.AttemptKey()
	if !s.begin(key) {
		return false, nil
	}
	defer s.end(key)
	record, err := s.load(c, t, session)
	if err != nil || s.deferred(record) {
		return false, err
	}
	if record.Phase == "consumed" {
		_, err = c.ackSupplement(ctx, &record)
		return err == nil, err
	}
	progress, err := c.currentSupplement(ctx, &record)
	if err != nil || record.Current == nil {
		return progress, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	if err = c.fenceSupplement(ctx, t); err != nil {
		return false, err
	}
	if err = deliver(ctx, supplementMessageID(record, record.Current.CommentID), formatSupplement(*record.Current)); err != nil {
		return false, err
	}
	record.Phase = "consumed"
	if err = s.save(record); err != nil {
		return true, err
	}
	_, err = c.ackSupplement(ctx, &record)
	return true, err
}

// currentSupplement leaves record.Current as the native claim to deliver, or nil.
// The native receipt is the authority: a claim response is only a hint, so a lost
// response is read back instead of being claimed again.
func (c *Client) currentSupplement(ctx context.Context, record *supplementRecord) (bool, error) {
	view, err := c.runningSupplements(ctx, record, "")
	if err != nil {
		return false, err
	}
	if record.Phase == "claimed" {
		if view.Delivering != nil && view.Delivering.CommentID == record.Current.CommentID {
			return false, nil
		}
		if view, err = c.runningSupplements(ctx, record, record.Current.CommentID); err != nil {
			return false, err
		}
		if view.Receipt.Status == "delivering" {
			return false, nil
		}
		// The receipt left delivery before consumption, for example with its
		// deleted comment: nothing was consumed and nothing is acknowledged.
		record.Current, record.Phase = nil, "idle"
		return true, c.supplements.save(*record)
	}
	if view.Delivering == nil {
		claimed, claimErr := c.claimSupplement(ctx, record)
		if claimErr != nil || !claimed {
			return false, claimErr
		}
		if view, err = c.runningSupplements(ctx, record, ""); err != nil {
			return false, err
		}
		if view.Delivering == nil {
			record.Phase = "idle"
			return true, c.supplements.save(*record)
		}
	}
	if !view.Delivering.valid() {
		// Keep a valid claim intent while rejecting an undeliverable payload;
		// the payload itself cannot enter the validated delivery journal.
		record.Current, record.Phase = nil, "claiming"
		err = c.taskPost(ctx, record.Task, "supplements/"+view.Delivering.CommentID+"/ack", map[string]any{"delivered": false, "error": "provider_rejected"})
		if err != nil && !Conflict(err) {
			return false, c.supplements.delay(record, err)
		}
		record.Phase = "idle"
		return true, c.supplements.save(*record)
	}
	record.Current, record.Phase = view.Delivering, "claimed"
	record.Failures, record.RetryAfter = 0, 0
	return false, c.supplements.save(*record)
}

// claimSupplement persists intent before the claim. An ambiguous outcome keeps
// the claiming phase; the next round reads the native receipt before any claim.
func (c *Client) claimSupplement(ctx context.Context, record *supplementRecord) (bool, error) {
	record.Phase = "claiming"
	if err := c.supplements.save(*record); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		record.Phase = "idle"
		return false, errors.Join(err, c.supplements.save(*record))
	}
	var message taskSupplement
	err := c.call(ctx, http.MethodPost, "/api/daemon/tasks/"+record.Task+"/supplements/claim", map[string]any{"capabilities": []string{"task-supplement-author-v1"}}, &message)
	if err != nil {
		var rejection *HTTPError
		if errors.As(err, &rejection) && rejection.Status >= 400 && rejection.Status < 500 && rejection.Status != http.StatusRequestTimeout {
			record.Phase = "idle"
		}
		return false, c.supplements.delay(record, err)
	}
	record.Failures, record.RetryAfter = 0, 0
	if message == (taskSupplement{}) {
		record.Phase = "idle"
		return false, c.supplements.save(*record)
	}
	return true, nil
}

func supplementMessageID(record supplementRecord, comment string) string {
	sum := sha256.Sum256([]byte(record.Server + "\x00" + record.key() + "\x00" + record.Session + "\x00" + comment))
	return "msg_" + hex.EncodeToString(sum[:16])
}

// ackSupplement reports whether native delivery was acknowledged. A nil error
// without it means the receipt can no longer be acknowledged and was quarantined.
func (c *Client) ackSupplement(ctx context.Context, record *supplementRecord) (bool, error) {
	err := c.taskPost(ctx, record.Task, "supplements/"+record.Current.CommentID+"/ack", map[string]any{"delivered": true})
	if err != nil {
		acked, err := c.failedSupplementAck(ctx, record, err)
		if err != nil || !acked {
			return false, err
		}
	}
	record.Current, record.Phase, record.Failures, record.RetryAfter = nil, "idle", 0, 0
	return true, c.supplements.save(*record)
}
