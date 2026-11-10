package multica

import (
	"context"
	"errors"
	"net/http"
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

// failedSupplementAck resolves a refused acknowledgement from the exact native
// receipt. Only a receipt that provably left delivery is quarantined; any other
// refusal keeps the consumption evidence for a backed-off retry.
func (c *Client) failedSupplementAck(ctx context.Context, record *supplementRecord, cause error) (bool, error) {
	var rejection *HTTPError
	if !errors.As(cause, &rejection) || rejection.Status < 400 || rejection.Status >= 500 || Transient(cause) {
		return false, c.supplements.delay(record, cause)
	}
	view, err := c.inspectSupplement(ctx, record.Task, record.Current.CommentID)
	if supplementGone(err) {
		return false, errors.Join(cause, c.removeSupplement(*record))
	}
	if err != nil {
		return false, c.supplements.delay(record, errors.Join(cause, err))
	}
	terminal, receipt := terminalSupplementStatus(view.Status), view.Receipt
	switch {
	case receipt.Status == "delivered":
		return true, nil
	case receipt.Status == "delivering" || receipt.Status == "failed" && receipt.FailureReason == "turn_ended" && receipt.Attempts > 0:
		// Natively still acknowledgeable: the refusal was not about this receipt.
	case rejection.Status == http.StatusConflict || terminal:
		reason := "receipt_" + receipt.Status
		if err = c.supplements.quarantine(*record, reason); err != nil {
			return false, c.supplements.delay(record, errors.Join(cause, err))
		}
		c.logSupplementQuarantine(*record, reason)
		if terminal {
			return false, c.removeSupplement(*record)
		}
		record.Current, record.Phase, record.Failures, record.RetryAfter = nil, "idle", 0, 0
		return false, c.supplements.save(*record)
	}
	return false, c.supplements.delay(record, cause)
}

// SupplementRetryDelay is a local scheduling hint, never delivery authority.
// Persisted retries run at their deadline instead of waiting for idle polling.
func (c *Client) SupplementRetryDelay(t Task, session string) (time.Duration, bool) {
	if c.supplements == nil || !c.SupplementsNegotiated(t) {
		return 0, false
	}
	record, err := c.supplements.load(c, t, session)
	if err != nil || record.RetryAfter == 0 {
		return 0, false
	}
	return max(time.Second, time.Unix(0, record.RetryAfter).Sub(c.supplements.now())), true
}
