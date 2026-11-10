package multica

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
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
	if err != nil {
		return false, err
	}
	if record.Phase == "consumed" {
		return c.ackSupplement(ctx, &record)
	}
	status, err := c.Status(ctx, t.ID)
	if err != nil {
		if Missing(err) {
			s.retire(t.ID)
		}
		return false, err
	}
	if status != "running" {
		if terminalSupplementStatus(status) {
			s.retire(t.ID)
		} else {
			return false, ErrSupplementUncertain
		}
		return false, ErrSupplementEnded
	}
	if record.Phase == "idle" || record.Phase == "claiming" {
		if err = c.claimSupplement(ctx, t, &record); err != nil {
			if err == ErrSupplementEnded {
				s.retire(t.ID)
			}
			return false, err
		}
		if record.Current == nil {
			return false, nil
		}
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	if err = c.fenceSupplement(ctx, t); err != nil {
		return false, err
	}
	if !slices.Contains(record.Done, record.Current.CommentID) {
		id := supplementMessageID(record, record.Current.CommentID)
		if err = deliver(ctx, id, formatSupplement(*record.Current)); err != nil {
			return false, err
		}
	}
	record.Phase = "consumed"
	if err = s.save(record); err != nil {
		return true, err
	}
	return c.ackSupplement(ctx, &record)
}

func (c *Client) claimSupplement(ctx context.Context, t Task, record *supplementRecord) error {
	views, err := c.inspectSupplements(ctx, t)
	if err != nil {
		return err
	}
	unknown := []taskSupplement{}
	pending := false
	for _, view := range views {
		if view.Status == "delivering" && !slices.Contains(record.Done, view.Message.CommentID) {
			unknown = append(unknown, view.Message)
		}
		pending = pending || view.Status == "pending"
	}
	if record.Phase == "claiming" {
		// An empty inspection cannot prove the old HTTP request will not commit
		// later. Never issue another claim while that outcome is ambiguous.
		if len(unknown) != 1 {
			return ErrSupplementUncertain
		}
		record.Current = &unknown[0]
	} else {
		if len(unknown) != 0 || len(record.Done) >= 4096 {
			return ErrSupplementUncertain
		}
		if !pending {
			return nil
		}
		record.Phase = "claiming"
		if err = c.supplements.save(*record); err != nil {
			return err
		}
		var message taskSupplement
		err = c.call(ctx, http.MethodPost, "/api/daemon/tasks/"+t.ID+"/supplements/claim", map[string]any{"capabilities": []string{"task-supplement-author-v1"}}, &message)
		if err != nil {
			return err
		}
		if message.CommentID == "" {
			if message != (taskSupplement{}) {
				return ErrSupplementUncertain
			}
			record.Phase = "idle"
			return c.supplements.save(*record)
		}
		if !message.valid() {
			return ErrSupplementUncertain
		}
		record.Current = &message
	}
	record.Phase = "claimed"
	return c.supplements.save(*record)
}

func supplementMessageID(record supplementRecord, comment string) string {
	sum := sha256.Sum256([]byte(record.Server + "\x00" + record.key() + "\x00" + record.Session + "\x00" + comment))
	return "msg_" + hex.EncodeToString(sum[:16])
}

func (c *Client) ackSupplement(ctx context.Context, record *supplementRecord) (bool, error) {
	if err := c.taskPost(ctx, record.Task, "supplements/"+record.Current.CommentID+"/ack", map[string]any{"delivered": true}); err != nil {
		return false, err
	}
	if !slices.Contains(record.Done, record.Current.CommentID) {
		record.Done = append(record.Done, record.Current.CommentID)
	}
	record.Current, record.Phase = nil, "idle"
	return true, c.supplements.save(*record)
}
