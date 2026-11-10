package multica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ReplaySupplementAcks never injects guidance after restart. Proven consumption
// is replayed subject to persisted backoff; all other rows settle natively.
func (c *Client) ReplaySupplementAcks(ctx context.Context) (int, error) {
	s := c.supplements
	if s == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	delivered := 0
	var errs error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := s.read(c.base, entry.Name())
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if !s.begin(record.key()) {
			continue
		}
		n, err := c.replaySupplement(ctx, entry.Name())
		s.end(record.key())
		delivered += n
		errs = errors.Join(errs, err)
	}
	return delivered, errs
}

func (c *Client) replaySupplement(ctx context.Context, name string) (int, error) {
	s := c.supplements
	record, err := s.read(c.base, name)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if s.deferred(record) {
		return 0, nil
	}
	delivered := 0
	if record.Phase == "consumed" {
		if _, err := c.ackSupplement(resent(ctx), &record); err != nil {
			return 0, err
		}
		delivered++
	}
	status, err := c.Status(ctx, record.Task)
	if err != nil {
		if supplementGone(err) {
			return delivered, c.removeSupplement(record)
		}
		return delivered, s.delay(&record, err)
	}
	if terminalSupplementStatus(status) {
		s.retire(record.Task)
		return delivered, s.remove(record.key())
	}
	return delivered, nil
}

func terminalSupplementStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func (s *supplementStore) read(server, name string) (supplementRecord, error) {
	var record supplementRecord
	data, err := os.ReadFile(s.pathName(name))
	if err != nil {
		return record, err
	}
	if len(data) > supplementJournalLimit || json.Unmarshal(data, &record) != nil || !record.valid(server) || name != fileName(record.key()) {
		return record, fmt.Errorf("invalid supplement journal")
	}
	return record, nil
}

func (s *supplementStore) pathName(name string) string {
	return s.dir + string(os.PathSeparator) + name
}

func (s *supplementStore) retire(task string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, id := range s.negotiated {
		if id == task {
			delete(s.negotiated, key)
			delete(s.wake, key)
		}
	}
}
