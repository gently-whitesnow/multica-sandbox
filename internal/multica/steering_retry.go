package multica

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

func (s *supplementStore) deferred(record supplementRecord) bool {
	return record.RetryAfter > s.now().UnixNano()
}

func (s *supplementStore) delay(record *supplementRecord, cause error) error {
	if record.Failures < 8 {
		record.Failures++
	}
	delay := min(4*time.Second<<uint(record.Failures-1), 5*time.Minute)
	record.RetryAfter = s.now().Add(delay).UnixNano()
	return errors.Join(cause, s.save(*record))
}

func supplementGone(err error) bool {
	return Missing(err) || statusIs(err, http.StatusGone)
}

func (c *Client) removeSupplement(record supplementRecord) error {
	c.supplements.retire(record.Task)
	return c.supplements.remove(record.key())
}

func (c *Client) failedSupplementAck(ctx context.Context, record *supplementRecord, cause error) error {
	var rejection *HTTPError
	if errors.As(cause, &rejection) && rejection.Status >= 400 && rejection.Status < 500 && !Transient(cause) {
		status, err := c.Status(ctx, record.Task)
		if supplementGone(err) {
			return errors.Join(cause, c.removeSupplement(*record))
		}
		if err == nil && terminalSupplementStatus(status) {
			// Retain consumption evidence for inspection, outside the automatic
			// replay set. A permanent refusal is never called a delivered receipt.
			s := c.supplements
			err = os.Rename(s.path(record.key()), s.path(record.key())+".quarantine")
			if err == nil {
				err = syncSupplementDir(s.dir)
			}
			if err == nil {
				s.retire(record.Task)
				return fmt.Errorf("terminal supplement acknowledgement quarantined: %w", cause)
			}
			cause = errors.Join(cause, err)
		}
	}
	return c.supplements.delay(record, cause)
}
