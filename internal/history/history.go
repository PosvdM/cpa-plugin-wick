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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-wick/internal/keepalive"
	"github.com/PosvdM/cpa-plugin-wick/internal/pricing"
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

// CountByKey counts the pings from from to now by conversation key. The
// status polls it every few seconds, so it skips the sorting of Query.
func (s *Store) CountByKey(from time.Time) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return nil, errors.New("history directory not set")
	}
	now := s.now()
	counts := make(map[string]int)
	for day := from.UTC().Truncate(24 * time.Hour); !day.After(now); day = day.Add(24 * time.Hour) {
		events, err := s.readDayLocked(s.fileFor(day))
		if err != nil {
			return nil, err
		}
		for _, ev := range events {
			if ev.Key != "" && ev.Kind == "" && !ev.Time.Before(from) {
				counts[ev.Key]++
			}
		}
	}
	return counts, nil
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

// Round results.
const (
	// ResultSaved: the request came back while the cache of the last
	// successful ping was alive, after the cache would have expired
	// without the pings.
	ResultSaved = "saved"
	// ResultWasted: no request came back within the cache's lifetime after
	// the last successful ping, or it came back before the cache would have
	// expired anyway.
	ResultWasted = "wasted"
	// ResultPending: the cache of the last successful ping is alive and no
	// request has come back yet.
	ResultPending = "pending"
	// ResultMiss and ResultFailed: the last ping wrote the cache or failed.
	ResultMiss   = "miss"
	ResultFailed = "failed"
	// ResultUnknown: written before returns were recorded.
	ResultUnknown = "unknown"
)

// cacheTTL and hourTTL are how long a 5-minute and a 1-hour prompt cache
// entry live after they were last read.
const (
	cacheTTL = 5 * time.Minute
	hourTTL  = time.Hour
)

// Totals sums pings and rounds.
type Totals struct {
	Pings               int   `json:"pings"`
	Failed              int   `json:"failed"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	Rounds              int   `json:"rounds"`
	// Results counts the rounds by result.
	Results map[string]int `json:"results"`
	// Cost is the price of the pings and Saved the cache writes saved, in
	// US dollars at API prices. Net is Saved minus the cost of the rounds
	// with a result, leaving out pending ones. Rounds with ResultUnknown
	// and rounds whose model has no price are left out of all three;
	// Unpriced counts the latter.
	Cost     float64 `json:"cost"`
	Saved    float64 `json:"saved"`
	Net      float64 `json:"net"`
	Unpriced int     `json:"unpriced"`
}

// addRound adds r; priced reports whether prices were available, so that
// rounds are counted as unpriced only when their model has no price.
func (t *Totals) addRound(r Round, priced bool) {
	t.Rounds++
	if t.Results == nil {
		t.Results = make(map[string]int)
	}
	t.Results[r.Result]++
	if r.Result == ResultUnknown {
		return
	}
	if r.Cost == nil {
		if priced {
			t.Unpriced++
		}
		return
	}
	saved := 0.0
	if r.Saved != nil {
		saved = *r.Saved
	}
	t.Cost += *r.Cost
	t.Saved += saved
	if r.Result != ResultPending {
		t.Net += saved - *r.Cost
	}
}

func (t *Totals) add(o Totals) {
	t.Pings += o.Pings
	t.Failed += o.Failed
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheCreationTokens += o.CacheCreationTokens
	t.Rounds += o.Rounds
	if t.Results == nil {
		t.Results = make(map[string]int)
	}
	for k, v := range o.Results {
		t.Results[k] += v
	}
	t.Cost += o.Cost
	t.Saved += o.Saved
	t.Net += o.Net
	t.Unpriced += o.Unpriced
}

// Total sums the totals of conversations.
func Total(convs []Conversation) Totals {
	t := Totals{Results: map[string]int{}}
	for _, c := range convs {
		t.add(c.Totals)
	}
	return t
}

// Round is the pings of a conversation between two of its requests.
type Round struct {
	Start  time.Time `json:"start"`
	Last   time.Time `json:"last"`
	Model  string    `json:"model"`
	Pings  int       `json:"pings"`
	Failed int       `json:"failed"`
	// Context is the input of the last ping that reported usage.
	Context         int64  `json:"context"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	Result          string `json:"result"`
	// Return is when the request that ended the round came, and Idle the
	// seconds from the request before it.
	Return *time.Time `json:"return,omitempty"`
	Idle   int64      `json:"idle,omitempty"`
	// Cost and Saved are in US dollars at the current API prices, nil when
	// the model has no price. Saved is set for ResultSaved only.
	Cost  *float64 `json:"cost,omitempty"`
	Saved *float64 `json:"saved,omitempty"`

	cost     float64
	priced   bool
	lastRead time.Time
	// last is the outcome of the last ping: empty for a hit, ResultMiss or
	// ResultFailed.
	last     string
	judged   bool
	returned bool
	saved    bool
	// savedContext is the context recorded with the return.
	savedContext int64
	lastPing     int
}

// Conversation sums the pings of one conversation in a range.
type Conversation struct {
	Key     string `json:"key"`
	Session string `json:"session"`
	// Model, Agent, Client, Cwd and Prompt are those of the latest ping.
	Model  string    `json:"model"`
	Agent  bool      `json:"agent,omitempty"`
	Client string    `json:"client,omitempty"`
	Cwd    string    `json:"cwd,omitempty"`
	Prompt string    `json:"prompt,omitempty"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	Totals
	// Context is the input of the latest ping that reported usage: cached,
	// written and uncached tokens.
	Context int64 `json:"context"`
	// LastError and LastStopped are those of the latest ping.
	LastError   string `json:"last_error,omitempty"`
	LastStopped string `json:"last_stopped,omitempty"`
	// Detail holds the rounds, newest first.
	Detail []Round `json:"detail"`
}

// Group sums events, oldest first, by conversation, newest conversation
// first, and splits each conversation into rounds, judged at now and priced
// with prices, which may be nil. Events without a conversation key,
// written before keys were recorded, are grouped by session.
//
// A ping numbered no higher than the one before it starts a new round, as
// does the first ping after a return, so that rounds are rebuilt from any
// history. A return without a ping of its round in the range is left out.
func Group(events []keepalive.Event, now time.Time, prices *pricing.Table) []Conversation {
	index := make(map[string]int)
	var out []Conversation
	// open marks the conversations whose last round has no return yet.
	open := make(map[int]bool)
	for _, ev := range events {
		id := ev.Key
		if id == "" {
			id = "session:" + ev.Session
		}
		if ev.Kind == keepalive.KindRequest {
			continue
		}
		i, ok := index[id]
		if ev.Kind == keepalive.KindReturn {
			if ok && open[i] {
				c := &out[i]
				r := &c.Detail[len(c.Detail)-1]
				t := ev.Time
				r.Return, r.Idle = &t, ev.Idle
				r.returned, r.saved, r.savedContext = true, ev.Saved, ev.Context
				open[i] = false
			}
			continue
		}
		if !ok {
			i = len(out)
			index[id] = i
			out = append(out, Conversation{Key: ev.Key, Session: ev.Session, First: ev.Time})
		}
		c := &out[i]
		c.Model, c.Agent, c.Client, c.Cwd, c.Prompt = ev.Model, ev.Agent, ev.Client, ev.Cwd, ev.Prompt
		c.Last, c.LastError, c.LastStopped = ev.Time, ev.Error, ev.Stopped
		c.Pings++
		if !open[i] || ev.Ping <= c.Detail[len(c.Detail)-1].lastPing {
			c.Detail = append(c.Detail, Round{Start: ev.Time, priced: true})
			open[i] = true
		}
		r := &c.Detail[len(c.Detail)-1]
		r.Last, r.Model, r.lastPing, r.judged = ev.Time, ev.Model, ev.Ping, ev.Returns
		r.Pings++
		switch {
		case ev.Error != "":
			c.Failed++
			r.Failed++
			r.last = ResultFailed
		case ev.Stopped == "cache_miss" || (ev.Usage != nil && (ev.Usage.CacheReadTokens <= 0 || ev.Usage.CacheCreationTokens > 0)):
			r.last = ResultMiss
		default:
			r.last = ""
			r.lastRead = ev.Time
		}
		if ev.Usage != nil {
			u := ev.Usage
			context := u.CacheReadTokens + u.CacheCreationTokens + u.InputTokens
			c.CacheReadTokens += u.CacheReadTokens
			c.CacheCreationTokens += u.CacheCreationTokens
			c.Context, r.Context = context, context
			r.CacheReadTokens += u.CacheReadTokens
			// The stream is closed at message_start, so output is not
			// counted.
			if rates, ok := prices.Lookup(ev.Model, context); ok {
				r.cost += pricing.Cost(u.CacheReadTokens, rates.Read) + pricing.Cost(u.CacheCreationTokens, rates.Write5m) + pricing.Cost(u.InputTokens, rates.Input)
			} else {
				r.priced = false
			}
		}
	}
	for i := range out {
		c := &out[i]
		for j := range c.Detail {
			r := &c.Detail[j]
			r.judge(now, prices)
			c.addRound(*r, prices != nil)
		}
		slices.Reverse(c.Detail)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// judge sets the result and the amounts of r at now.
func (r *Round) judge(now time.Time, prices *pricing.Table) {
	// A return comes first: a ping still in flight when the request came
	// back may miss or fail after the return was judged.
	switch {
	case r.returned && r.saved:
		r.Result = ResultSaved
	case r.returned:
		r.Result = ResultWasted
	case r.last != "":
		r.Result = r.last
	case !r.judged:
		r.Result = ResultUnknown
	case now.Before(r.lastRead.Add(cacheTTL)):
		r.Result = ResultPending
	default:
		r.Result = ResultWasted
	}
	if prices == nil || !r.priced {
		return
	}
	cost := r.cost
	r.Cost = &cost
	if r.Result == ResultSaved {
		// The request that came back read the context from the cache
		// instead of writing it again.
		if rates, ok := prices.Lookup(r.Model, r.savedContext); ok {
			saved := pricing.Cost(r.savedContext, rates.Write5m-rates.Read)
			r.Saved = &saved
		} else {
			r.Cost = nil
		}
	}
}

// Comparison prices the cache of the conversations Wick keeps lit three
// ways, in US dollars at API prices: with Wick (Cache plus Pings), without
// it (Cache plus Saved), and with a 1-hour cache and no pings (Hour).
// Uncached input and output cost the same in all three and are left out.
type Comparison struct {
	// Since is the first request with usage in the range. Rounds that
	// ended before it, by their return or last ping, are left out; it is
	// nil when there is none.
	Since    *time.Time `json:"since,omitempty"`
	Requests int        `json:"requests"`
	// Counts cover the same rounds as the comparison, including unpriced rounds.
	PingCount int            `json:"ping_count"`
	Failed    int            `json:"failed"`
	Rounds    int            `json:"rounds"`
	Results   map[string]int `json:"results"`
	// Wick and None are full cache costs. Differences are positive for savings.
	Wick           float64  `json:"wick"`
	None           float64  `json:"none"`
	Difference     float64  `json:"difference"`
	Percent        *float64 `json:"percent,omitempty"`
	HourDifference float64  `json:"hour_difference"`
	// Cache is what the requests paid for cache reads and 5-minute writes.
	Cache float64 `json:"cache"`
	// Pings is the cost of the pings and Saved the cache writes they saved.
	Pings float64 `json:"pings"`
	Saved float64 `json:"saved"`
	Hour  float64 `json:"hour"`
	// Unpriced counts the requests and rounds whose model has no price.
	Unpriced int `json:"unpriced"`
}

// Compare prices the requests among events and the rounds of convs, both
// grouped from the same events, three ways. It returns an empty Comparison
// when prices is nil.
//
// With a 1-hour cache a request reads and writes the same tokens as it did,
// writes priced at the 1-hour rate, with two exceptions. A request that
// came 5 minutes to an hour after the previous one and read less than that
// request's input found the 5-minute cache expired; the 1-hour cache would
// still hold that input. A request that came back more than an hour later
// to a cache kept lit by Wick would find the 1-hour cache expired and write
// the context again.
func Compare(events []keepalive.Event, convs []Conversation, prices *pricing.Table) Comparison {
	var out Comparison
	if prices == nil {
		return out
	}
	for _, ev := range events {
		if ev.Kind != keepalive.KindRequest || ev.Usage == nil {
			continue
		}
		if out.Since == nil {
			t := ev.Time
			out.Since = &t
		}
		u := ev.Usage
		cached := u.CacheReadTokens + u.CacheCreationTokens
		rates, ok := prices.Lookup(ev.Model, cached+u.InputTokens)
		if !ok {
			out.Unpriced++
			continue
		}
		out.Requests++
		out.Cache += pricing.Cost(u.CacheReadTokens, rates.Read) + pricing.Cost(u.CacheCreationTokens, rates.Write5m)
		read := u.CacheReadTokens
		if ev.Idle >= int64(cacheTTL/time.Second) && ev.Context > read {
			read = min(ev.Context, cached)
		}
		out.Hour += pricing.Cost(read, rates.Read) + pricing.Cost(cached-read, rates.Write1h)
	}
	if out.Since == nil {
		return out
	}
	for _, c := range convs {
		for _, r := range c.Detail {
			end := r.Last
			if r.Return != nil {
				end = *r.Return
			}
			if end.Before(*out.Since) {
				continue
			}
			out.PingCount += r.Pings
			out.Failed += r.Failed
			out.Rounds++
			if out.Results == nil {
				out.Results = make(map[string]int)
			}
			out.Results[r.Result]++
			if r.Result == ResultUnknown {
				continue
			}
			if r.Cost == nil {
				out.Unpriced++
				continue
			}
			out.Pings += *r.Cost
			if r.Saved == nil {
				continue
			}
			out.Saved += *r.Saved
			if r.Idle >= int64(hourTTL/time.Second) {
				if rates, ok := prices.Lookup(r.Model, r.savedContext); ok {
					out.Hour += pricing.Cost(r.savedContext, rates.Write1h-rates.Read)
				}
			}
		}
	}
	out.Wick, out.None = out.Cache+out.Pings, out.Cache+out.Saved
	out.Difference = out.None - out.Wick
	out.HourDifference = out.Hour - out.Wick
	if out.None > 0 {
		percent := out.Difference / out.None * 100
		out.Percent = &percent
	}
	return out
}
