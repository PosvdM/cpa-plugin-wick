// Package pricing reads the Claude API prices from the Markdown version of
// Anthropic's pricing page, so that new models are priced without a new
// release, and keeps the last good copy in the plugin's data directory.
package pricing

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source is the Markdown version of the official pricing page.
const Source = "https://platform.claude.com/docs/en/about-claude/pricing.md"

// minModels is the fewest models a parsed page must list. Fewer means the
// page changed its layout, and the cached prices are kept instead.
const minModels = 5

// Rates are prices in US dollars per million tokens.
type Rates struct {
	Input   float64 `json:"input"`
	Write5m float64 `json:"write_5m"`
	Write1h float64 `json:"write_1h"`
	Read    float64 `json:"read"`
	Output  float64 `json:"output"`
}

// Tier holds the rates for prompts of at most UpTo tokens. UpTo 0 has no
// upper bound. A prompt counts every input token: cached, written and
// uncached.
type Tier struct {
	UpTo int64 `json:"up_to,omitempty"`
	Rates
}

// Model is one model of the pricing table. Family holds the words of the
// name between "Claude" and the version, such as "opus", and Version its
// number, such as "4.5".
type Model struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Family  string `json:"family"`
	Version string `json:"version"`
	// Tiers are ordered by UpTo, the unbounded tier last.
	Tiers []Tier `json:"tiers"`
}

// Table is a parsed pricing page.
type Table struct {
	Fetched time.Time `json:"fetched"`
	Source  string    `json:"source"`
	Models  []Model   `json:"models"`
}

var (
	pricePattern = regexp.MustCompile(`\$\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*/\s*MTok`)
	// notePattern matches a parenthesized note after a model name, such as
	// "(for prompts up to 100,000 tokens)" or "([limited availability](…))".
	notePattern = regexp.MustCompile(`\s*\((?:[^()]|\([^()]*\))*\)`)
	tierPattern = regexp.MustCompile(`(?i)prompts\s+(up to|over)\s+([0-9][0-9,]*)\s+tokens`)
	wordPattern = regexp.MustCompile(`[A-Za-z]+|[0-9]+(?:\.[0-9]+)*`)
)

// The columns of the model pricing table, found by their headers.
var columns = []struct {
	header string
	set    func(*Rates, float64)
}{
	{"base input", func(r *Rates, v float64) { r.Input = v }},
	{"5m cache write", func(r *Rates, v float64) { r.Write5m = v }},
	{"1h cache write", func(r *Rates, v float64) { r.Write1h = v }},
	{"cache hit", func(r *Rates, v float64) { r.Read = v }},
	{"output", func(r *Rates, v float64) { r.Output = v }},
}

// Parse reads the first table under the "## Model pricing" heading.
func Parse(page []byte) ([]Model, error) {
	lines := strings.Split(strings.ReplaceAll(string(page), "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), "## Model pricing") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, errors.New("pricing page has no Model pricing section")
	}
	var rows [][]string
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			break
		}
		if strings.HasPrefix(line, "|") {
			rows = append(rows, splitRow(line))
		} else if len(rows) > 0 {
			break
		}
	}
	if len(rows) < 3 {
		return nil, errors.New("pricing page has no model table")
	}
	index := make([]int, len(columns))
	for c, col := range columns {
		index[c] = -1
		for i, cell := range rows[0] {
			if strings.Contains(strings.ToLower(cell), col.header) {
				index[c] = i
				break
			}
		}
		if index[c] < 0 {
			return nil, fmt.Errorf("pricing table has no %q column", col.header)
		}
	}

	byID := make(map[string]int)
	var models []Model
	for _, row := range rows[2:] {
		if len(row) == 0 {
			continue
		}
		name, upTo, ok := parseName(row[0])
		if !ok {
			continue
		}
		var tier Tier
		tier.UpTo = upTo
		complete := true
		for c, col := range columns {
			if index[c] >= len(row) {
				complete = false
				break
			}
			v, ok := parsePrice(row[index[c]])
			if !ok {
				complete = false
				break
			}
			col.set(&tier.Rates, v)
		}
		if !complete {
			continue
		}
		family, version := nameKey(name)
		if family == "" {
			continue
		}
		id := modelID(family, version)
		i, seen := byID[id]
		if !seen {
			i = len(models)
			byID[id] = i
			models = append(models, Model{Name: name, ID: id, Family: family, Version: version})
		}
		models[i].Tiers = append(models[i].Tiers, tier)
	}
	for i := range models {
		sortTiers(models[i].Tiers)
	}
	if len(models) < minModels {
		return nil, fmt.Errorf("pricing table lists %d models, expected at least %d", len(models), minModels)
	}
	return models, nil
}

func splitRow(line string) []string {
	line = strings.TrimPrefix(strings.TrimSuffix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// parseName returns the model name without its notes and, for a row priced
// by prompt length, the tier bound: the threshold for "up to", -threshold
// for "over".
func parseName(cell string) (string, int64, bool) {
	var upTo int64
	for _, note := range notePattern.FindAllString(cell, -1) {
		if m := tierPattern.FindStringSubmatch(note); m != nil {
			n, err := strconv.ParseInt(strings.ReplaceAll(m[2], ",", ""), 10, 64)
			if err != nil || n <= 0 {
				return "", 0, false
			}
			if strings.EqualFold(m[1], "over") {
				n = -n
			}
			upTo = n
		}
	}
	name := strings.Join(strings.Fields(notePattern.ReplaceAllString(cell, " ")), " ")
	if !strings.HasPrefix(strings.ToLower(name), "claude ") {
		return "", 0, false
	}
	return name, upTo, true
}

func parsePrice(cell string) (float64, bool) {
	m := pricePattern.FindStringSubmatch(cell)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	return v, err == nil && v >= 0
}

// sortTiers orders bounded tiers by their bound and turns the "over" tier
// into the unbounded one.
func sortTiers(tiers []Tier) {
	for i := range tiers {
		if tiers[i].UpTo < 0 {
			tiers[i].UpTo = 0
		}
	}
	sort.SliceStable(tiers, func(i, j int) bool {
		a, b := tiers[i].UpTo, tiers[j].UpTo
		return a != 0 && (b == 0 || a < b)
	})
}

// nameKey splits a display name such as "Claude Opus 4.5" into its family
// "opus" and version "4.5".
func nameKey(name string) (string, string) {
	var family []string
	version := ""
	for _, w := range wordPattern.FindAllString(name, -1) {
		switch {
		case w[0] >= '0' && w[0] <= '9':
			if version == "" {
				version = w
			}
		case strings.EqualFold(w, "claude"):
		default:
			family = append(family, strings.ToLower(w))
		}
	}
	return strings.Join(family, "-"), trimVersion(version)
}

func modelID(family, version string) string {
	id := "claude-" + family
	if version != "" {
		id += "-" + strings.ReplaceAll(version, ".", "-")
	}
	return id
}

// Lookup returns the rates of model for a prompt of the given number of
// tokens. The model name is matched by family and version, so that dates,
// "[1m]" and other suffixes are ignored and both word orders match:
// claude-3-5-haiku-20241022 and claude-haiku-3-5 are Claude Haiku 3.5.
func (t *Table) Lookup(model string, prompt int64) (Rates, bool) {
	if t == nil {
		return Rates{}, false
	}
	words, version := modelKey(model)
	if len(words) == 0 {
		return Rates{}, false
	}
	var best *Model
	bestWords := 0
	for i := range t.Models {
		m := &t.Models[i]
		if m.Version != version || len(m.Tiers) == 0 {
			continue
		}
		family := strings.Split(m.Family, "-")
		if containsAll(words, family) && len(family) > bestWords {
			best, bestWords = m, len(family)
		}
	}
	if best == nil {
		return Rates{}, false
	}
	for _, tier := range best.Tiers {
		if tier.UpTo == 0 || prompt <= tier.UpTo {
			return tier.Rates, true
		}
	}
	return best.Tiers[len(best.Tiers)-1].Rates, true
}

// modelKey splits a model ID into its words and version: lowercase, without
// a bracketed suffix such as "[1m]" and without date stamps.
func modelKey(model string) ([]string, string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(model, '['); i >= 0 {
		model = model[:i]
	}
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	var words, numbers []string
	ended := false
	for _, part := range strings.FieldsFunc(model, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ' ' || r == '@' || r == ':' }) {
		switch {
		case isDigits(part):
			// The version is the first run of short numbers. Dates such as
			// 20250805 and later numbers, such as the 0 of a Bedrock
			// "-v1:0" suffix, are not part of it.
			if n, err := strconv.Atoi(part); err == nil && len(part) <= 2 && !ended {
				numbers = append(numbers, strconv.Itoa(n))
			} else if len(numbers) > 0 {
				ended = true
			}
		default:
			if len(numbers) > 0 {
				ended = true
			}
			if part != "claude" {
				words = append(words, part)
			}
		}
	}
	return words, trimVersion(strings.Join(numbers, "."))
}

// trimVersion drops trailing ".0" parts, so that 4.0 and 4 are the same.
func trimVersion(v string) string {
	for strings.HasSuffix(v, ".0") {
		v = strings.TrimSuffix(v, ".0")
	}
	return v
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func containsAll(words, want []string) bool {
	for _, w := range want {
		found := false
		for _, have := range words {
			if have == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Cost returns the price in dollars of tokens at a rate per million tokens.
func Cost(tokens int64, perMTok float64) float64 {
	return float64(tokens) * perMTok / 1e6
}
