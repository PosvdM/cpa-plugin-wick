package keepalive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// keyPattern matches a conversation key, which also names its request file.
var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	// indexFile holds the schedule of every saved conversation. The request
	// of each conversation is saved in <key>.req next to it.
	indexFile = "index.json"
	// requestExt is the extension of the saved request files.
	requestExt = ".req"
	// saveDelay batches the changes of a burst of requests into one write.
	saveDelay = 5 * time.Second
	// restoreDelay leaves CPA time to finish loading the plugin before a
	// restored conversation is pinged.
	restoreDelay = 3 * time.Second
	// restoreMargin is the time a restored ping needs to reach upstream
	// before the cache entry expires.
	restoreMargin = 10 * time.Second
)

// convStore saves the tracked conversations under dir, so that a new plugin
// instance continues their keep-alive after CPA restarts or reloads the
// plugin. A goroutine writes the files after a change, off the request path.
type convStore struct {
	m      *Manager
	dir    string
	delay  time.Duration
	signal chan struct{}
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
	// saved maps a conversation key to the gen of the request in its file,
	// so that a request is written once.
	savedMu sync.Mutex
	saved   map[string]uint64
}

// savedIndex is the content of indexFile.
type savedIndex struct {
	Conversations []savedConversation `json:"conversations"`
}

type savedConversation struct {
	Key         string    `json:"key"`
	FirstSeen   time.Time `json:"first_seen"`
	LastRequest time.Time `json:"last_request"`
	LastRead    time.Time `json:"last_read"`
	NextPing    time.Time `json:"next_ping"`
	Pings       int       `json:"pings"`
	Minutes     int       `json:"minutes,omitempty"`
	LastUsage   *Usage    `json:"last_usage,omitempty"`

	gen uint64
	req request
}

// savedRequest is the first line of a request file. The request body
// follows it unchanged, so that the replay stays byte for byte the same.
type savedRequest struct {
	Session   string      `json:"session"`
	Agent     string      `json:"agent,omitempty"`
	Key       string      `json:"key"`
	Model     string      `json:"model"`
	AuthID    string      `json:"auth_id"`
	RequestID string      `json:"request_id"`
	Client    string      `json:"client,omitempty"`
	Cwd       string      `json:"cwd,omitempty"`
	Prompt    string      `json:"prompt,omitempty"`
	Headers   http.Header `json:"headers"`
}

func newConvStore(m *Manager, dir string) *convStore {
	return &convStore{
		m:      m,
		dir:    dir,
		delay:  saveDelay,
		signal: make(chan struct{}, 1),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		saved:  make(map[string]uint64),
	}
}

func (s *convStore) start() { go s.run() }

// changed asks the goroutine to save the conversations.
func (s *convStore) changed() {
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// stop tells the goroutine to exit without writing again.
func (s *convStore) stop() { s.once.Do(func() { close(s.quit) }) }

// wait returns once the goroutine has exited, so that a write in progress
// cannot overwrite a later one. It must not be called while holding the
// manager's lock, which the goroutine takes for a snapshot.
func (s *convStore) wait() { <-s.done }

func (s *convStore) run() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.m.host.Log("error", fmt.Sprintf("wick: saving conversations panicked: %v", r), nil)
		}
	}()
	for {
		select {
		case <-s.quit:
			return
		case <-s.signal:
		}
		select {
		case <-s.quit:
			return
		case <-time.After(s.delay):
		}
		convs, ok := s.m.snapshot(s)
		if !ok {
			return
		}
		if err := s.write(convs); err != nil {
			s.m.host.Log("warn", "wick: save conversations: "+err.Error(), nil)
		}
	}
}

// snapshot copies the state of the conversations for s, or returns false
// when s is no longer the manager's store.
func (m *Manager) snapshot(s *convStore) ([]savedConversation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != s || m.stopped {
		return nil, false
	}
	return m.snapshotLocked(), true
}

func (m *Manager) snapshotLocked() []savedConversation {
	convs := make([]savedConversation, 0, len(m.convs))
	for key, c := range m.convs {
		sc := savedConversation{
			Key:         key,
			FirstSeen:   c.firstSeen,
			LastRequest: c.lastRequest,
			LastRead:    c.lastRead,
			NextPing:    c.nextPing,
			Pings:       c.pings,
			LastUsage:   c.lastUsage,
			Minutes:     c.minutes,
			gen:         c.gen,
			// The request is never modified after parseRequest, so
			// sharing its body and headers is safe.
			req: c.req,
		}
		convs = append(convs, sc)
	}
	return convs
}

// write saves the index, the requests that changed since the last write,
// and deletes the files of conversations that ended.
func (s *convStore) write(convs []savedConversation) error {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	keep := map[string]bool{indexFile: true}
	live := make(map[string]bool, len(convs))
	for _, c := range convs {
		name := c.Key + requestExt
		keep[name] = true
		live[c.Key] = true
		if gen, ok := s.saved[c.Key]; ok && gen == c.gen {
			continue
		}
		if err := writeFileAtomic(filepath.Join(s.dir, name), encodeRequest(c.req)); err != nil {
			return err
		}
		s.saved[c.Key] = c.gen
	}
	raw, err := json.Marshal(savedIndex{Conversations: convs})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, indexFile), raw); err != nil {
		return err
	}
	for key := range s.saved {
		if !live[key] {
			delete(s.saved, key)
		}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && !keep[entry.Name()] {
			os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
	return nil
}

func encodeRequest(req request) []byte {
	head, _ := json.Marshal(savedRequest{
		Session:   req.Session,
		Agent:     req.Agent,
		Key:       req.Key,
		Model:     req.Model,
		AuthID:    req.AuthID,
		RequestID: req.RequestID,
		Client:    req.Client,
		Cwd:       req.Cwd,
		Prompt:    req.Prompt,
		Headers:   req.Headers,
	})
	out := make([]byte, 0, len(head)+1+len(req.Body))
	out = append(out, head...)
	out = append(out, '\n')
	return append(out, req.Body...)
}

func decodeRequest(raw []byte) (request, error) {
	head, body, ok := bytes.Cut(raw, []byte{'\n'})
	if !ok {
		return request{}, errors.New("no request body")
	}
	var saved savedRequest
	if err := json.Unmarshal(head, &saved); err != nil {
		return request{}, err
	}
	return request{
		Session:   saved.Session,
		Agent:     saved.Agent,
		Key:       saved.Key,
		Model:     saved.Model,
		AuthID:    saved.AuthID,
		RequestID: saved.RequestID,
		Client:    saved.Client,
		Cwd:       saved.Cwd,
		Prompt:    saved.Prompt,
		Headers:   saved.Headers,
		Body:      body,
	}, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// restoreLocked loads the saved conversations whose cache is still alive and
// schedules their next ping at the saved time, but no earlier than
// restoreDelay after loading and no later than restoreMargin before the
// cache expires.
func (m *Manager) restoreLocked() {
	s := m.store
	raw, err := os.ReadFile(filepath.Join(s.dir, indexFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.host.Log("warn", "wick: read saved conversations: "+err.Error(), nil)
		}
		return
	}
	var index savedIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		m.host.Log("warn", "wick: read saved conversations: "+err.Error(), nil)
		return
	}
	now := m.clock.Now()
	expired := 0
	for _, sc := range index.Conversations {
		if _, ok := m.convs[sc.Key]; ok || len(m.convs) >= maxConversations || !keyPattern.MatchString(sc.Key) {
			continue
		}
		earliest, latest := now.Add(restoreDelay), sc.LastRead.Add(cacheTTL-restoreMargin)
		if !earliest.Before(latest) {
			expired++
			continue
		}
		at := sc.NextPing
		if at.Before(earliest) {
			at = earliest
		}
		// interval_max can be up to 299 s, later than restoreMargin allows.
		if at.After(latest) {
			at = latest
		}
		file, err := os.ReadFile(filepath.Join(s.dir, sc.Key+requestExt))
		if err != nil {
			m.host.Log("warn", "wick: read saved conversation: "+err.Error(), nil)
			continue
		}
		req, err := decodeRequest(file)
		if err != nil || req.Key != sc.Key {
			m.host.Log("warn", "wick: invalid saved conversation "+short(sc.Key), nil)
			continue
		}
		c := &conversation{
			req:         req,
			firstSeen:   sc.FirstSeen,
			lastRequest: sc.LastRequest,
			lastRead:    sc.LastRead,
			pings:       sc.Pings,
			lastUsage:   sc.LastUsage,
			minutes:     sc.Minutes,
		}
		// The duration may have been shortened while Wick was not running.
		if m.doneLocked(c) {
			continue
		}
		m.gen++
		c.gen = m.gen
		m.convs[sc.Key] = c
		s.saved[sc.Key] = c.gen
		m.scheduleAtLocked(sc.Key, c, at)
		m.logLocked("info", "saved conversation restored", req, c.pings, "")
	}
	if expired > 0 {
		m.host.Log("info", fmt.Sprintf("wick: %d saved conversations expired while Wick was not running", expired), nil)
	}
}
