// Package pricing derives session cost from token counters for providers
// that record usage but not dollars.
//
// Hermes reports `estimated_cost_usd` directly, so it never needs this.
// Claude Code and Codex record only token counts, so their adapters must
// price the tokens themselves. A model we have no rates for yields no cost
// at all rather than a misleading zero — see agent.CostSource.
package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Rates are per-million-token prices in USD, plus the multipliers that
// relate cached tokens to the base input rate.
type Rates struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`

	// CacheReadMult and CacheWriteMult scale the input rate. Cache reads are
	// roughly a tenth of base input; a cache write carries a premium for the
	// privilege of storing the prefix.
	CacheReadMult  float64 `json:"cache_read_mult"`
	CacheWriteMult float64 `json:"cache_write_mult"`

	// ReasoningPerMTok prices reasoning tokens. Providers that bill
	// reasoning at the output rate leave this zero and inherit OutputPerMTok.
	ReasoningPerMTok float64 `json:"reasoning_per_mtok,omitempty"`
}

// Standard cache multipliers: reads cost ~0.1x base input, and a 5-minute
// cache write costs 1.25x (a 1-hour write costs 2x, but adapters cannot
// currently distinguish the two from recorded usage, so we price the common
// 5-minute case and mark the whole figure as an estimate).
const (
	cacheReadMult  = 0.10
	cacheWriteMult = 1.25
)

func std(in, out float64) Rates {
	return Rates{
		InputPerMTok:   in,
		OutputPerMTok:  out,
		CacheReadMult:  cacheReadMult,
		CacheWriteMult: cacheWriteMult,
	}
}

// introOffer is a promotional rate that reverts on a known date.
type introOffer struct {
	rates Rates
	until time.Time
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 23, 59, 59, 0, time.UTC)
}

// table holds first-party Anthropic rates. Every entry is keyed by the exact
// model ID; normalize() maps provider-prefixed and suffixed variants onto
// these keys.
//
// Models absent from this table are not guessed at — Lookup reports them as
// unknown so the UI can say so plainly. Users add their own rates (OpenRouter
// models, self-hosted endpoints, anything not listed) via the override file.
var table = map[string]Rates{
	"claude-fable-5":    std(10, 50),
	"claude-mythos-5":   std(10, 50),
	"claude-opus-5":     std(5, 25),
	"claude-opus-4-8":   std(5, 25),
	"claude-opus-4-7":   std(5, 25),
	"claude-opus-4-6":   std(5, 25),
	"claude-sonnet-5":   std(3, 15),
	"claude-sonnet-4-6": std(3, 15),
	"claude-haiku-4-5":  std(1, 5),
}

// intro holds promotional pricing that supersedes the table until it lapses.
var intro = map[string]introOffer{
	"claude-sonnet-5": {rates: std(2, 10), until: date(2026, time.August, 31)},
}

// fastMode prices models running under fast mode, which bills at a premium.
var fastMode = map[string]Rates{
	"claude-opus-5":   std(10, 50),
	"claude-opus-4-8": std(10, 50),
}

// windows holds each model's context window in tokens. Providers that
// self-report a window (Codex) never consult this; Claude Code does not
// report one, so its adapter looks the model up here.
var windows = map[string]int64{
	"claude-fable-5":    1_000_000,
	"claude-mythos-5":   1_000_000,
	"claude-opus-5":     1_000_000,
	"claude-opus-4-8":   1_000_000,
	"claude-opus-4-7":   1_000_000,
	"claude-opus-4-6":   1_000_000,
	"claude-sonnet-5":   1_000_000,
	"claude-sonnet-4-6": 1_000_000,
	"claude-haiku-4-5":  200_000,
}

// ContextWindow returns a model's context window and whether it is known.
func ContextWindow(model string) (int64, bool) {
	n, ok := windows[normalize(model)]
	return n, ok
}

// overrides holds user-supplied rates loaded from disk, which win over the
// built-in table so a user can price models we do not ship rates for.
var overrides map[string]Rates

// LoadOverrides reads user-supplied rates from path. A missing file is not an
// error — it just means no overrides. Pass an empty path for the default
// location (~/.config/sloppy-toppy/pricing.json).
func LoadOverrides(path string) error {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = filepath.Join(home, ".config", "sloppy-toppy", "pricing.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var parsed map[string]Rates
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	// Fill in cache multipliers the user omitted, so a minimal override file
	// listing only input/output prices still prices cached tokens sanely, and
	// normalize the keys so matching does not silently depend on the casing
	// the user happened to type.
	loaded := make(map[string]Rates, len(parsed))
	for name, r := range parsed {
		if r.CacheReadMult == 0 {
			r.CacheReadMult = cacheReadMult
		}
		if r.CacheWriteMult == 0 {
			r.CacheWriteMult = cacheWriteMult
		}
		loaded[normalize(name)] = r
	}
	overrides = loaded
	return nil
}

// dateSuffix matches a trailing snapshot date such as "-20251001".
//
// Anchored on exactly eight digits at the end so it cannot eat the version
// segments of an id like claude-haiku-4-5.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// normalize maps a provider-decorated model string onto a table key.
//
// It strips the vendor prefix Bedrock and OpenRouter-style IDs carry
// ("anthropic.claude-opus-5", "anthropic/claude-opus-5"), the context-window
// suffix Claude Code appends ("claude-opus-5[1m]"), and a trailing date
// snapshot ("claude-haiku-4-5-20251001" — which appears in live transcripts
// and previously missed the table entirely, leaving the session unpriced).
//
// It deliberately does NOT strip "-fast", which selects real premium pricing
// and is handled separately.
func normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexAny(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimPrefix(m, "anthropic.")
	if i := strings.Index(m, "["); i >= 0 {
		m = m[:i]
	}
	return dateSuffix.ReplaceAllString(m, "")
}

// Lookup returns the rates for a model and whether any were found.
func Lookup(model string, now time.Time) (Rates, bool) {
	m := normalize(model)

	// User overrides win outright, including over promotional pricing. Keys
	// were normalized at load, so this single lookup is case-insensitive.
	if r, ok := overrides[m]; ok {
		return r, true
	}

	if fast := strings.TrimSuffix(m, "-fast"); fast != m {
		if r, ok := fastMode[fast]; ok {
			return r, true
		}
		m = fast
	}

	if offer, ok := intro[m]; ok && now.Before(offer.until) {
		return offer.rates, true
	}
	if r, ok := table[m]; ok {
		return r, true
	}
	return Rates{}, false
}

// Usage is the token breakdown a cost is computed from.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
}

// Cost prices a usage breakdown in USD.
func Cost(u Usage, r Rates) float64 {
	const perMTok = 1_000_000.0
	reasoningRate := r.ReasoningPerMTok
	if reasoningRate == 0 {
		reasoningRate = r.OutputPerMTok
	}
	return (float64(u.Input)*r.InputPerMTok +
		float64(u.Output)*r.OutputPerMTok +
		float64(u.Reasoning)*reasoningRate +
		float64(u.CacheRead)*r.InputPerMTok*r.CacheReadMult +
		float64(u.CacheWrite)*r.InputPerMTok*r.CacheWriteMult) / perMTok
}

// Estimate prices a usage breakdown for a named model, reporting whether the
// model's rates were known. A false return means the caller must present the
// cost as unavailable rather than as zero.
func Estimate(model string, u Usage, now time.Time) (float64, bool) {
	r, ok := Lookup(model, now)
	if !ok {
		return 0, false
	}
	return Cost(u, r), true
}
