// Package history stores the result of every ping in the plugin's data
// directory, one JSON Lines file per UTC day, and answers the queries of
// the management page.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
)

const dayLayout = "2006-01-02"

// Store appends ping events to day files under a directory and deletes the
// files older than the retention.
type Store struct {
	mu        sync.Mutex
	dir       string
	retention int
	// prunedDay is the UTC day of the last pruning.
	prunedDay string
	// cache holds the parsed day files, checked by size and modification
	// time before use.
	cache map[string]cachedDay
	now   func() time.Time
}

type cachedDay struct {
	size    int64
	modTime time.Time
	events  []keepalive.Event
}

// New returns a store that is not configured yet.
func New() *Store {
	return &Store{cache: make(map[string]cachedDay), now: time.Now}
}

// Configure sets the directory and the number of days to keep.
func (s *Store) Configure(dir string, retentionDays int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != dir {
		s.cache = make(map[string]cachedDay)
	}
	s.dir, s.retention, s.prunedDay = dir, retentionDays, ""
}

// Dir returns the directory of the day files.
func (s *Store) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// Retention returns the number of days kept.
func (s *Store) Retention() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retention
}

func (s *Store) fileFor(t time.Time) string {
	return filepath.Join(s.dir, t.UTC().Format(dayLayout)+".jsonl")
}

// Append writes ev to the file of its day. The first write of a day also
// deletes the files older than the retention.
func (s *Store) Append(ev keepalive.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return errors.New("history directory not set")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// One write with O_APPEND per line keeps lines whole when two plugin
	// instances overlap during a reload.
	file, err := os.OpenFile(s.fileFor(ev.Time), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(raw, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if today := s.now().UTC().Format(dayLayout); today != s.prunedDay {
		s.prunedDay = today
		s.pruneLocked()
	}
	return err
}

// Query returns the events from from to now, oldest first.
func (s *Store) Query(from time.Time) ([]keepalive.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return nil, errors.New("history directory not set")
	}
	now := s.now()
	var out []keepalive.Event
	for day := from.UTC().Truncate(24 * time.Hour); !day.After(now); day = day.Add(24 * time.Hour) {
		events, err := s.readDayLocked(s.fileFor(day))
		if err != nil {
			return nil, err
		}
		for _, ev := range events {
			if !ev.Time.Before(from) {
				out = append(out, ev)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func (s *Store) readDayLocked(path string) ([]keepalive.Event, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		delete(s.cache, path)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c, ok := s.cache[path]; ok && c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
		return c.events, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []keepalive.Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev keepalive.Event
		if json.Unmarshal(scanner.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	s.cache[path] = cachedDay{size: info.Size(), modTime: info.ModTime(), events: events}
	return events, nil
}

// pruneLocked deletes the day files older than the retention.
func (s *Store) pruneLocked() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := s.now().UTC().AddDate(0, 0, -s.retention).Truncate(24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day, err := time.Parse(dayLayout, strings.TrimSuffix(name, ".jsonl"))
		if err != nil || !day.Before(cutoff) {
			continue
		}
		path := filepath.Join(s.dir, name)
		os.Remove(path)
		delete(s.cache, path)
	}
}

// Totals sums a list of events.
type Totals struct {
	Pings               int   `json:"pings"`
	Failed              int   `json:"failed"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
}

// Sum counts the pings, failures and cache tokens of events.
func Sum(events []keepalive.Event) Totals {
	var t Totals
	for _, ev := range events {
		t.Pings++
		if ev.Error != "" {
			t.Failed++
		}
		if ev.Usage != nil {
			t.CacheReadTokens += ev.Usage.CacheReadTokens
			t.CacheCreationTokens += ev.Usage.CacheCreationTokens
		}
	}
	return t
}
