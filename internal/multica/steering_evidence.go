package multica

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Quarantined delivery evidence records consumption that Multica can no longer
// acknowledge. It is controller-private, never replayed and unrelated to the
// OpenCode database quarantine inside session volumes. One entry is at most one
// journal record (64 KiB of guidance in practice, 1 MiB by bound), so the byte
// budget holds several hundred entries; 30 days cover an incident review while
// the native task history still explains the refusal.
const (
	supplementEvidenceDir      = "quarantine"
	supplementEvidenceAge      = 30 * 24 * time.Hour
	supplementEvidenceEntries  = 512
	supplementEvidenceBytes    = 64 << 20
	supplementEvidenceInterval = 10 * time.Minute
	supplementTemp             = ".supplement-"
	supplementTempAge          = time.Hour
)

type supplementEvidence struct {
	Version       int              `json:"version"`
	Reason        string           `json:"reason"`
	QuarantinedAt time.Time        `json:"quarantined_at"`
	Record        supplementRecord `json:"record"`
}

// quarantine durably writes the evidence before the caller releases the journal;
// a crash in between repeats the same refusal and rewrites the same entry.
func (s *supplementStore) quarantine(record supplementRecord, reason string) error {
	data, err := json.Marshal(supplementEvidence{1, reason, s.now().UTC(), record})
	if err != nil || len(data) > supplementJournalLimit+4096 {
		return fmt.Errorf("supplement evidence exceeds its bound")
	}
	s.evidence.Lock()
	defer s.evidence.Unlock()
	dir := filepath.Join(s.dir, supplementEvidenceDir)
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err = realSupplementDir(dir); err != nil {
		return err
	}
	if err = writeSupplementFile(dir, fileName(record.key()+":"+record.Current.CommentID), data); err != nil {
		return err
	}
	return syncSupplementDir(s.dir)
}

func realSupplementDir(dir string) error {
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return errors.Join(fmt.Errorf("supplement quarantine is not a directory"), err)
	}
	return nil
}

func (c *Client) logSupplementQuarantine(record supplementRecord, reason string) {
	if c.log != nil {
		c.log.Warn("supplement consumption quarantined: Multica no longer accepts its receipt", "task", record.Task, "comment", record.Current.CommentID, "reason", reason)
	}
}

// collectSupplementEvidence bounds the quarantine at cold start and then at
// most once per interval. It is synchronous, so shutdown never interrupts it.
func (c *Client) collectSupplementEvidence() {
	s := c.supplements
	now := s.now()
	s.mu.Lock()
	due := s.collected.IsZero() || now.Before(s.collected) || now.Sub(s.collected) >= supplementEvidenceInterval
	if due {
		s.collected = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	removed, freed, err := s.collect(now)
	if c.log != nil && err != nil {
		c.log.Warn("supplement quarantine collection failed", "error", err.Error())
	}
	if c.log != nil && removed > 0 {
		c.log.Info("supplement quarantine collected", "removed", removed, "freed_bytes", freed)
	}
}

type supplementEvidenceFile struct {
	name     string
	modified time.Time
	size     int64
}

// collect removes expired entries, then the oldest beyond the entry and byte
// budgets, and stale temporary files. It reads names and metadata only, so a
// corrupted entry ages out like any other. Active journals live one directory
// up and are never candidates; symlinks, directories and foreign names are
// neither followed nor removed.
func (s *supplementStore) collect(now time.Time) (int, int64, error) {
	s.evidence.Lock()
	defer s.evidence.Unlock()
	_, errs := supplementFiles(s.dir, now)
	dir := filepath.Join(s.dir, supplementEvidenceDir)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return 0, 0, errs
	}
	if err := realSupplementDir(dir); err != nil {
		return 0, 0, errors.Join(errs, err)
	}
	files, err := supplementFiles(dir, now)
	errs = errors.Join(errs, err)
	sort.Slice(files, func(i, j int) bool {
		if !files[i].modified.Equal(files[j].modified) {
			return files[i].modified.After(files[j].modified)
		}
		return files[i].name < files[j].name
	})
	removed, kept, bytes, freed, full := 0, 0, int64(0), int64(0), false
	for _, file := range files {
		full = full || now.Sub(file.modified) > supplementEvidenceAge || kept >= supplementEvidenceEntries || bytes+file.size > supplementEvidenceBytes
		if !full {
			kept, bytes = kept+1, bytes+file.size
			continue
		}
		if err = os.Remove(filepath.Join(dir, file.name)); err != nil && !os.IsNotExist(err) {
			errs = errors.Join(errs, err)
			continue
		}
		removed, freed = removed+1, freed+file.size
	}
	if removed > 0 {
		errs = errors.Join(errs, syncSupplementDir(dir))
	}
	return removed, freed, errs
}

// supplementFiles lists dir's regular *.json files and removes its stale
// temporary files, which only an interrupted write leaves behind.
func supplementFiles(dir string, now time.Time) ([]supplementEvidenceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []supplementEvidenceFile
	var errs error
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		switch {
		case strings.HasPrefix(entry.Name(), supplementTemp):
			if now.Sub(info.ModTime()) > supplementTempAge {
				if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
					errs = errors.Join(errs, err)
				}
			}
		case supplementEvidenceName(entry.Name()):
			files = append(files, supplementEvidenceFile{entry.Name(), info.ModTime(), info.Size()})
		}
	}
	return files, errs
}

// Evidence names use the journal's SHA-256 spelling; unrelated JSON is private
// operator data, not an entry owned by this collector.
func supplementEvidenceName(name string) bool {
	if len(name) != 64+len(".json") || !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := hex.DecodeString(name[:64])
	return err == nil && name[:64] == strings.ToLower(name[:64])
}
