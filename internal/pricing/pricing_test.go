package pricing

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func loadTable(t *testing.T) *Table {
	t.Helper()
	page, err := os.ReadFile("testdata/pricing.md")
	if err != nil {
		t.Fatal(err)
	}
	models, err := Parse(page)
	if err != nil {
		t.Fatal(err)
	}
	return &Table{Models: models}
}

// The prices of the official page on 2026-10-24, in $/MTok.
func TestParseOfficialPage(t *testing.T) {
	table := loadTable(t)
	cases := []struct {
		model  string
		prompt int64
		want   Rates
	}{
		{"claude-fable-5-1", 50000, Rates{10, 12.5, 20, 0.25, 50}},
		{"claude-opus-5-5", 50000, Rates{4, 5, 8, 0.20, 20}},
		{"claude-opus-4-8", 50000, Rates{5, 6.25, 10, 0.50, 25}},
		{"claude-sonnet-5-5", 50000, Rates{2, 2.5, 4, 0.10, 10}},
		{"claude-haiku-5-5", 100000, Rates{0.10, 0.125, 0.20, 0.01, 0.50}},
		{"claude-haiku-5-5", 100001, Rates{0.50, 0.625, 1, 0.05, 2.50}},
		// Suffixes, dates and the old word order.
		{"claude-opus-5-5[1m]", 50000, Rates{4, 5, 8, 0.20, 20}},
		{"Claude-Opus-5-5-Thinking", 50000, Rates{4, 5, 8, 0.20, 20}},
		{"claude-opus-4-1-20250805", 50000, Rates{15, 18.75, 30, 1.50, 75}},
		{"claude-opus-4-20250514", 50000, Rates{15, 18.75, 30, 1.50, 75}},
		{"claude-3-5-haiku-20241022", 50000, Rates{0.80, 1, 1.60, 0.08, 4}},
		// Aliases with a trailing zero, Bedrock IDs and later numbers.
		{"claude-opus-4-0", 50000, Rates{15, 18.75, 30, 1.50, 75}},
		{"us.anthropic.claude-opus-4-5-20251101-v1:0", 50000, Rates{5, 6.25, 10, 0.50, 25}},
		{"claude-opus-5-5-high-2", 50000, Rates{4, 5, 8, 0.20, 20}},
		// A note in parentheses is not part of the name.
		{"claude-mythos-5-1", 50000, Rates{10, 12.5, 20, 0.25, 50}},
	}
	for _, c := range cases {
		got, ok := table.Lookup(c.model, c.prompt)
		if !ok || got != c.want {
			t.Errorf("%s at %d: got %+v %v, want %+v", c.model, c.prompt, got, ok, c.want)
		}
	}
	for _, model := range []string{"claude-3-7-sonnet-20250219", "gpt-5", "claude-opus-9", ""} {
		if _, ok := table.Lookup(model, 1); ok {
			t.Errorf("%s matched a price", model)
		}
	}
	if _, ok := (*Table)(nil).Lookup("claude-opus-5-5", 1); ok {
		t.Error("nil table matched a price")
	}
}

func TestParseNamesAndIDs(t *testing.T) {
	table := loadTable(t)
	ids := map[string]string{}
	for _, m := range table.Models {
		ids[m.ID] = m.Name
	}
	for id, name := range map[string]string{
		"claude-opus-5-5":   "Claude Opus 5.5",
		"claude-mythos-5-1": "Claude Mythos 5.1",
		"claude-haiku-5-5":  "Claude Haiku 5.5",
		"claude-haiku-3-5":  "Claude Haiku 3.5",
	} {
		if ids[id] != name {
			t.Errorf("%s: name %q, want %q", id, ids[id], name)
		}
	}
}

func TestParseRejectsChangedPage(t *testing.T) {
	if _, err := Parse([]byte("# Pricing\n\nNothing here.\n")); err == nil {
		t.Error("page without the section parsed")
	}
	few := "## Model pricing\n\n| Model | Base input tokens | 5m cache writes | 1h cache writes | Cache hits and refreshes | Output tokens |\n| - | - | - | - | - | - |\n| Claude Opus 5.5 | $4 / MTok | $5 / MTok | $8 / MTok | $0.20 / MTok | $20 / MTok |\n"
	if _, err := Parse([]byte(few)); err == nil || !strings.Contains(err.Error(), "at least") {
		t.Errorf("one model: err %v", err)
	}
	renamed := strings.Replace(few, "Output tokens", "Completion", 1)
	if _, err := Parse([]byte(renamed)); err == nil {
		t.Error("table without an output column parsed")
	}
}

func TestStoreKeepsLastGoodPrices(t *testing.T) {
	page, _ := os.ReadFile("testdata/pricing.md")
	serve := page
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(serve) }))
	defer srv.Close()
	dir := t.TempDir()
	s := &Store{URL: srv.URL, Client: srv.Client(), dir: dir}
	quit := make(chan struct{})
	if err := s.Refresh(quit); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Fetched.IsZero() || st.Models < 10 || st.Error != "" {
		t.Fatalf("state %+v", st)
	}
	// A page that no longer parses keeps the prices and reports the error.
	serve = []byte("## Model pricing\n\nmoved\n")
	if err := s.Refresh(quit); err == nil {
		t.Fatal("bad page accepted")
	}
	if st := s.State(); st.Models < 10 || st.Error == "" {
		t.Fatalf("state after a bad page %+v", st)
	}
	// A new store reads the saved prices.
	other := &Store{URL: "http://127.0.0.1:1", Client: &http.Client{}}
	other.Start(dir)
	other.Stop()
	if _, ok := other.Table().Lookup("claude-opus-5-5", 1); !ok {
		t.Fatal("saved prices not loaded")
	}
}
