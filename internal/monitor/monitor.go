// Package monitor polls every configured provider, derives rates by
// diffing cumulative counters between polls, and classifies agent state.
package monitor

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/provider"
)

// Defaults are the single source of truth for tuning knobs. The CLI reads
// its flag defaults from here, so the values the tests exercise are the
// values the shipped binary uses — previously the Monitor and the CLI
// disagreed about ActiveIdle and nothing caught it.
const (
	DefaultActiveIdle  = 60 * time.Second
	DefaultStuckIdle   = 240 * time.Second
	DefaultRefresh     = 2 * time.Second
	DefaultSpendPeriod = 60 * time.Second

	// emaAlpha weights the newest sample in the rate EMA. Lower values
	// smooth harder; 0.3 tracks bursts without flickering.
	emaAlpha = 0.3
)

// Config tunes a Monitor.
type Config struct {
	// ActiveIdle is how long since last activity still counts as ACTIVE.
	ActiveIdle time.Duration
	// StuckIdle is how long a non-interactive session may idle before it is
	// considered wedged.
	StuckIdle time.Duration
	// ShowEnded includes finished sessions in snapshots.
	ShowEnded bool
}

// withDefaults fills zero values so a zero Config behaves sensibly.
func (c Config) withDefaults() Config {
	if c.ActiveIdle <= 0 {
		c.ActiveIdle = DefaultActiveIdle
	}
	if c.StuckIdle <= 0 {
		c.StuckIdle = DefaultStuckIdle
	}
	return c
}

// rateState is the per-session bookkeeping needed to turn cumulative
// counters into rates.
type rateState struct {
	prevTokens float64
	prevCost   float64
	emaTok     float64
	emaCost    float64
	seeded     bool // an EMA exists, so blend rather than replace
}

// Monitor polls providers and derives rates and state.
type Monitor struct {
	cfg       Config
	providers []provider.Provider
	spends    []provider.Spend

	mu          sync.Mutex
	rates       map[string]*rateState
	lastPoll    time.Time
	spendCache  map[string]*provider.Snapshot
	spendPolled map[string]time.Time
	spendPrev   map[string]float64
}

// New builds a Monitor over the given providers.
func New(cfg Config, providers []provider.Provider, spends []provider.Spend) *Monitor {
	return &Monitor{
		cfg:         cfg.withDefaults(),
		providers:   providers,
		spends:      spends,
		rates:       map[string]*rateState{},
		spendCache:  map[string]*provider.Snapshot{},
		spendPolled: map[string]time.Time{},
		spendPrev:   map[string]float64{},
	}
}

// Config returns the effective configuration.
func (m *Monitor) Config() Config { return m.cfg }

// SetShowEnded toggles inclusion of finished sessions.
func (m *Monitor) SetShowEnded(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.ShowEnded = v
}

// Snapshot polls every provider concurrently and returns the current agents
// with rates and state filled in.
//
// Providers are polled in parallel: they are independent, each does blocking
// file and sqlite I/O, and a slow one must not serialise behind the others.
// Errors are collected rather than fatal — a failing provider yields no rows
// instead of blanking the table.
func (m *Monitor) Snapshot(ctx context.Context, now time.Time) ([]*agent.Agent, []error) {
	type result struct {
		agents []*agent.Agent
		err    error
	}
	results := make([]result, len(m.providers))

	var wg sync.WaitGroup
	for i, p := range m.providers {
		wg.Add(1)
		go func(i int, p provider.Provider) {
			defer wg.Done()
			a, err := p.Poll(ctx, now)
			results[i] = result{agents: a, err: err}
		}(i, p)
	}
	wg.Wait()

	var agents []*agent.Agent
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
		}
		agents = append(agents, r.agents...)
	}

	m.mu.Lock()
	showEnded := m.cfg.ShowEnded
	m.mu.Unlock()

	if !showEnded {
		live := agents[:0]
		for _, a := range agents {
			if a.EndedAt.IsZero() {
				live = append(live, a)
			}
		}
		agents = live
	}

	m.deriveRates(agents, now)
	for _, a := range agents {
		m.Classify(a, now)
	}
	return agents, errs
}

// deriveRates turns cumulative counters into per-second and per-hour rates
// by diffing against the previous poll.
func (m *Monitor) deriveRates(agents []*agent.Agent, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var dt float64
	if !m.lastPoll.IsZero() {
		dt = now.Sub(m.lastPoll).Seconds()
	}

	seen := make(map[string]bool, len(agents))
	for _, a := range agents {
		key := a.Key()
		seen[key] = true
		tokens := float64(a.BurnTokens())
		cost := a.CostUSD

		st, existing := m.rates[key]
		if !existing {
			st = &rateState{}
			m.rates[key] = st
		}

		switch {
		case existing && dt > 0:
			// Normal path: diff against the previous poll. Counters are
			// monotonic, so a negative delta means a reset — clamp to zero
			// rather than reporting a negative burn rate.
			instTok := max0((tokens - st.prevTokens) / dt)
			instCost := max0((cost-st.prevCost)/dt) * 3600
			if st.seeded {
				st.emaTok = (1-emaAlpha)*st.emaTok + emaAlpha*instTok
				st.emaCost = (1-emaAlpha)*st.emaCost + emaAlpha*instCost
			} else {
				st.emaTok, st.emaCost = instTok, instCost
				st.seeded = true
			}
			a.TokRate, a.CostRate = st.emaTok, st.emaCost

		case existing && dt <= 0:
			// Two polls within the same instant (a hammered refresh key).
			// Hold the last rate steady. The previous implementation fell
			// through to the lifetime-average branch here, which made the
			// displayed rate visibly jump backwards.
			a.TokRate, a.CostRate = st.emaTok, st.emaCost

		default:
			// First sighting of this session: no baseline to diff against,
			// so seed from the lifetime average. Deliberately not stored as
			// an EMA, so the first real diff replaces it outright.
			if elapsed := now.Sub(a.StartedAt).Seconds(); !a.StartedAt.IsZero() && elapsed > 0 {
				a.TokRate = tokens / elapsed
				a.CostRate = cost / elapsed * 3600
			}
		}

		st.prevTokens, st.prevCost = tokens, cost
	}

	// Drop bookkeeping for sessions that are gone. Without this the maps
	// grow for the life of the process as sessions turn over.
	for key := range m.rates {
		if !seen[key] {
			delete(m.rates, key)
		}
	}
	m.lastPoll = now
}

func max0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// Classify assigns a lifecycle state to an agent.
func (m *Monitor) Classify(a *agent.Agent, now time.Time) {
	if !a.EndedAt.IsZero() {
		a.State = agent.StateDone
		return
	}
	if a.LastActivityAt.IsZero() {
		a.State = agent.StateIdle
		return
	}

	idle := now.Sub(a.LastActivityAt)
	if idle < 0 {
		idle = 0
	}
	a.IdleSeconds = idle.Seconds()

	switch {
	case idle <= m.cfg.ActiveIdle:
		a.State = agent.StateActive
	case containsWait(a.LastActionDesc):
		a.State = agent.StateWaiting
	case idle > m.cfg.StuckIdle && !a.IsInteractive():
		// Only non-interactive origins go STUCK. This tests the session's
		// own source, not the profile it happens to run under.
		a.State = agent.StateStuck
	default:
		a.State = agent.StateIdle
	}
}

// containsWait detects provider-written descriptions like "waiting for user
// approval (80s elapsed)", which mean the agent is blocked on a human rather
// than wedged.
func containsWait(desc string) bool {
	return strings.Contains(strings.ToLower(desc), "wait")
}

// SpendSnapshots fetches remote spend sources, honouring each source's own
// interval so a billing API is never polled at the table's refresh rate.
func (m *Monitor) SpendSnapshots(ctx context.Context, now time.Time) []*provider.Snapshot {
	var due []provider.Spend
	m.mu.Lock()
	for _, s := range m.spends {
		last, ok := m.spendPolled[s.Name()]
		if !ok || now.Sub(last) >= s.Interval() {
			due = append(due, s)
		}
	}
	m.mu.Unlock()

	if len(due) > 0 {
		fetched := make([]*provider.Snapshot, len(due))
		var wg sync.WaitGroup
		for i, s := range due {
			wg.Add(1)
			go func(i int, s provider.Spend) {
				defer wg.Done()
				snap, err := s.Fetch(ctx)
				if err != nil {
					fetched[i] = &provider.Snapshot{Source: s.Name(), Err: err, FetchedAt: now}
					return
				}
				if snap != nil {
					snap.Source = s.Name()
					snap.FetchedAt = now
				}
				fetched[i] = snap
			}(i, s)
		}
		wg.Wait()

		m.mu.Lock()
		for i, s := range due {
			m.spendPolled[s.Name()] = now
			snap := fetched[i]
			if snap == nil {
				// Unconfigured source: stay silent rather than reporting a
				// permanent failure.
				delete(m.spendCache, s.Name())
				continue
			}
			// Derive $/hr by diffing total spend between fetches.
			if prev, ok := m.spendPrev[s.Name()]; ok && snap.Err == nil {
				if elapsed := now.Sub(m.spendLast(s.Name())).Hours(); elapsed > 0 {
					snap.CostRate = max0(snap.SpentTotal-prev) / elapsed
				}
			}
			if snap.Err == nil {
				m.spendPrev[s.Name()] = snap.SpentTotal
			}
			m.spendCache[s.Name()] = snap
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*provider.Snapshot, 0, len(m.spendCache))
	for _, s := range m.spends {
		if snap, ok := m.spendCache[s.Name()]; ok {
			out = append(out, snap)
		}
	}
	return out
}

// spendLast returns when a source was previously fetched; callers hold m.mu.
func (m *Monitor) spendLast(name string) time.Time {
	if snap, ok := m.spendCache[name]; ok {
		return snap.FetchedAt
	}
	return time.Time{}
}
