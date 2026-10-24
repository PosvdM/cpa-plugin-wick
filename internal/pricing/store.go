package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// cacheFile holds the last good table in the data directory.
	cacheFile = "prices.json"
	// refreshInterval is the time between fetches of the pricing page.
	refreshInterval = 24 * time.Hour
	// retryInterval is the time before the next try after a failed fetch.
	retryInterval = time.Hour
	fetchTimeout  = 15 * time.Second
	maxPageBytes  = 4 << 20
)

// State is what the management page shows about the prices.
type State struct {
	// Fetched is when the prices in use were fetched; zero when there are
	// none.
	Fetched time.Time `json:"fetched"`
	Source  string    `json:"source"`
	Models  int       `json:"models"`
	// Error is the last failed fetch, cleared by the next good one.
	Error string `json:"error,omitempty"`
}

// Store fetches the pricing page when the plugin starts and once a day
// after that, and keeps the last good table in memory and on disk.
type Store struct {
	// URL and Client are used by fetch; tests replace them.
	URL    string
	Client *http.Client
	// Log writes to the CPA log. It may be nil.
	Log func(level, message string)

	mu      sync.Mutex
	dir     string
	table   *Table
	lastErr string
	quit    chan struct{}
}

// NewStore returns a store that fetches from Source.
func NewStore(log func(level, message string)) *Store {
	return &Store{URL: Source, Client: &http.Client{Timeout: fetchTimeout}, Log: log}
}

// Start sets the data directory, loads the prices saved there and starts
// the goroutine that fetches the page, unless it is already running.
func (s *Store) Start(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != dir {
		s.dir = dir
		if table, err := readCache(dir); err == nil {
			s.table = table
		} else if !errors.Is(err, os.ErrNotExist) {
			s.logf("warn", "wick: read saved prices: %v", err)
		}
	}
	if s.quit != nil {
		return
	}
	s.quit = make(chan struct{})
	go s.run(s.quit)
}

// Stop ends the goroutine. A fetch in progress finishes within its timeout
// but its result is not used.
func (s *Store) Stop() {
	s.mu.Lock()
	quit := s.quit
	s.quit = nil
	s.mu.Unlock()
	if quit != nil {
		close(quit)
	}
}

// Table returns the prices in use, or nil when none were ever fetched.
func (s *Store) Table() *Table {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.table
}

// State describes the prices in use and the last error.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Source: s.URL, Error: s.lastErr}
	if s.table != nil {
		st.Fetched, st.Models = s.table.Fetched, len(s.table.Models)
	}
	return st
}

func (s *Store) run(quit chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			s.logf("error", "wick: fetching prices panicked: %v", r)
		}
		// Let the next Start run the goroutine again.
		s.mu.Lock()
		if s.quit == quit {
			s.quit = nil
		}
		s.mu.Unlock()
	}()
	for {
		wait := refreshInterval
		if err := s.Refresh(quit); err != nil {
			wait = retryInterval
		}
		select {
		case <-quit:
			return
		case <-time.After(wait):
		}
	}
}

// Refresh fetches and parses the page. On success it replaces the table and
// saves it; on failure it keeps the table in use and records the error.
// Nothing changes once quit is closed.
func (s *Store) Refresh(quit <-chan struct{}) error {
	models, err := s.fetch()
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-quit:
		return errors.New("stopped")
	default:
	}
	if err != nil {
		s.lastErr = err.Error()
		s.logf("warn", "wick: update prices: %v", err)
		return err
	}
	table := &Table{Fetched: time.Now().UTC(), Source: Source, Models: models}
	s.table, s.lastErr = table, ""
	if s.dir != "" {
		if err := writeCache(s.dir, table); err != nil {
			s.logf("warn", "wick: save prices: %v", err)
		}
	}
	return nil
}

func (s *Store) fetch() ([]Model, error) {
	req, err := http.NewRequest(http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/markdown, text/plain;q=0.9")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing page returned %s", resp.Status)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, err
	}
	return Parse(page)
}

func (s *Store) logf(level, format string, args ...any) {
	if s.Log != nil {
		s.Log(level, fmt.Sprintf(format, args...))
	}
}

func readCache(dir string) (*Table, error) {
	raw, err := os.ReadFile(filepath.Join(dir, cacheFile))
	if err != nil {
		return nil, err
	}
	var table Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, err
	}
	if len(table.Models) == 0 {
		return nil, errors.New("saved prices list no models")
	}
	return &table, nil
}

func writeCache(dir string, table *Table) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, cacheFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
