package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/provider"
)

// fakeProvider returns a scripted set of agents, letting the monitor's rate
// and pruning logic be tested without touching any real runtime.
type fakeProvider struct {
	name   string
	agents []*agent.Agent
	err    error
	calls  int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Poll(context.Context, time.Time) ([]*agent.Agent, error) {
	f.calls++
	// Return copies so the monitor's mutations do not leak between polls.
	out := make([]*agent.Agent, len(f.agents))
	for i, a := range f.agents {
		c := *a
		out[i] = &c
	}
	return out, f.err
}

func newAgent(origin string, opts ...func(*agent.Agent)) *agent.Agent {
	a := &agent.Agent{
		Provider:       "test",
		Instance:       "inst",
		SessionID:      "s1",
		Origin:         origin,
		StartedAt:      time.Unix(1000, 0),
		LastActivityAt: time.Unix(2000, 0),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func TestClassify(t *testing.T) {
	now := time.Unix(3000, 0)
	m := New(Config{ActiveIdle: 60 * time.Second, StuckIdle: 240 * time.Second}, nil, nil)

	tests := []struct {
		name string
		a    *agent.Agent
		want agent.State
	}{
		{
			name: "ended session is done",
			a:    newAgent("cli", func(a *agent.Agent) { a.EndedAt = time.Unix(2500, 0) }),
			want: agent.StateDone,
		},
		{
			name: "recent activity is active",
			a:    newAgent("cli", func(a *agent.Agent) { a.LastActivityAt = time.Unix(2990, 0) }),
			want: agent.StateActive,
		},
		{
			name: "waiting description beats idle",
			a: newAgent("cli", func(a *agent.Agent) {
				a.LastActivityAt = time.Unix(2900, 0)
				a.LastActionDesc = "waiting for user approval (80s elapsed)"
			}),
			want: agent.StateWaiting,
		},
		{
			// The headline fix: a non-interactive origin idling past the
			// threshold is the case STUCK exists for.
			name: "non-interactive origin goes stuck",
			a:    newAgent("cron", func(a *agent.Agent) { a.LastActivityAt = time.Unix(2000, 0) }),
			want: agent.StateStuck,
		},
		{
			// An interactive chat thread idling for ages is an abandoned
			// conversation, not a wedged worker.
			name: "interactive origin never goes stuck",
			a:    newAgent("telegram", func(a *agent.Agent) { a.LastActivityAt = time.Unix(2000, 0) }),
			want: agent.StateIdle,
		},
		{
			name: "no activity timestamp is idle",
			a:    newAgent("cli", func(a *agent.Agent) { a.LastActivityAt = time.Time{} }),
			want: agent.StateIdle,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.Classify(tc.a, now)
			if tc.a.State != tc.want {
				t.Fatalf("state = %q, want %q", tc.a.State, tc.want)
			}
		})
	}
}

// TestClassifyIgnoresInstanceName pins the regression that motivated
// separating Origin from the display label: classification must key off the
// session's own source, never the profile it happens to run under.
func TestClassifyIgnoresInstanceName(t *testing.T) {
	now := time.Unix(3000, 0)
	m := New(Config{}, nil, nil)

	// A cron job running under a profile *named* "slack" must still be
	// eligible for STUCK.
	cronUnderSlackProfile := newAgent("cron", func(a *agent.Agent) {
		a.Instance = "slack"
		a.LastActivityAt = time.Unix(2000, 0)
	})
	m.Classify(cronUnderSlackProfile, now)
	if cronUnderSlackProfile.State != agent.StateStuck {
		t.Fatalf("cron under a slack-named profile: state = %q, want STUCK",
			cronUnderSlackProfile.State)
	}

	// And a genuine chat thread under the "default" profile must not be.
	chatUnderDefaultProfile := newAgent("slack", func(a *agent.Agent) {
		a.Instance = "default"
		a.LastActivityAt = time.Unix(2000, 0)
	})
	m.Classify(chatUnderDefaultProfile, now)
	if chatUnderDefaultProfile.State == agent.StateStuck {
		t.Fatal("slack thread under a default-named profile was marked STUCK")
	}
}

func TestRatesFromPollDiff(t *testing.T) {
	base := newAgent("cli", func(a *agent.Agent) {
		a.InputTokens = 100
		a.OutputTokens = 50
		a.ReasoningTokens = 10 // burn = 160
		a.CostUSD = 0.001
	})
	fp := &fakeProvider{name: "test", agents: []*agent.Agent{base}}
	m := New(Config{}, []provider.Provider{fp}, nil)
	ctx := context.Background()

	// First poll has no baseline, so rates seed from the lifetime average:
	// 160 tokens over 1000s of uptime.
	got, _ := m.Snapshot(ctx, time.Unix(2000, 0))
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1", len(got))
	}
	if want := 160.0 / 1000.0; !approx(got[0].TokRate, want, 1e-6) {
		t.Fatalf("seeded tok rate = %v, want %v", got[0].TokRate, want)
	}

	// Advance counters by 60 burn tokens over 2 seconds -> 30 tok/s.
	base.InputTokens = 160
	base.CostUSD = 0.0011
	got, _ = m.Snapshot(ctx, time.Unix(2002, 0))
	if !approx(got[0].TokRate, 30.0, 0.01) {
		t.Fatalf("diffed tok rate = %v, want 30", got[0].TokRate)
	}
	// $0.0001 over 2s is $0.18/hr.
	if !approx(got[0].CostRate, 0.18, 0.01) {
		t.Fatalf("diffed cost rate = %v, want 0.18", got[0].CostRate)
	}

	// With no further movement the EMA decays toward zero.
	got, _ = m.Snapshot(ctx, time.Unix(2004, 0))
	if got[0].TokRate >= 30.0 {
		t.Fatalf("rate did not decay: %v", got[0].TokRate)
	}
}

// TestZeroDeltaHoldsRate covers two polls landing in the same instant, which
// previously fell through to the lifetime-average branch and made the
// displayed rate visibly jump backwards.
func TestZeroDeltaHoldsRate(t *testing.T) {
	base := newAgent("cli", func(a *agent.Agent) {
		a.InputTokens = 100
		a.OutputTokens = 50
		a.ReasoningTokens = 10
	})
	fp := &fakeProvider{name: "test", agents: []*agent.Agent{base}}
	m := New(Config{}, []provider.Provider{fp}, nil)
	ctx := context.Background()

	m.Snapshot(ctx, time.Unix(2000, 0))
	base.InputTokens = 160
	got, _ := m.Snapshot(ctx, time.Unix(2002, 0))
	established := got[0].TokRate

	// Same timestamp: dt == 0.
	got, _ = m.Snapshot(ctx, time.Unix(2002, 0))
	if !approx(got[0].TokRate, established, 1e-9) {
		t.Fatalf("zero-delta poll changed rate: got %v, want %v held steady",
			got[0].TokRate, established)
	}
}

// TestRateStatePruned pins the leak: bookkeeping for sessions that have
// disappeared must not accumulate for the life of the process.
func TestRateStatePruned(t *testing.T) {
	fp := &fakeProvider{name: "test"}
	m := New(Config{}, []provider.Provider{fp}, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		fp.agents = []*agent.Agent{newAgent("cli", func(a *agent.Agent) {
			a.SessionID = string(rune('a' + i))
		})}
		m.Snapshot(ctx, time.Unix(int64(2000+i), 0))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rates) != 1 {
		t.Fatalf("rate state holds %d entries after 5 disjoint sessions, want 1", len(m.rates))
	}
}

func TestCounterResetDoesNotProduceNegativeRate(t *testing.T) {
	base := newAgent("cli", func(a *agent.Agent) { a.InputTokens = 1000 })
	fp := &fakeProvider{name: "test", agents: []*agent.Agent{base}}
	m := New(Config{}, []provider.Provider{fp}, nil)
	ctx := context.Background()

	m.Snapshot(ctx, time.Unix(2000, 0))
	base.InputTokens = 10 // counters reset beneath us
	got, _ := m.Snapshot(ctx, time.Unix(2002, 0))
	if got[0].TokRate < 0 {
		t.Fatalf("negative rate after counter reset: %v", got[0].TokRate)
	}
}

func TestEndedHiddenByDefault(t *testing.T) {
	ended := newAgent("cli", func(a *agent.Agent) { a.EndedAt = time.Unix(2500, 0) })
	fp := &fakeProvider{name: "test", agents: []*agent.Agent{ended}}

	m := New(Config{}, []provider.Provider{fp}, nil)
	if got, _ := m.Snapshot(context.Background(), time.Unix(3000, 0)); len(got) != 0 {
		t.Fatalf("ended session visible by default: %d rows", len(got))
	}

	m.SetShowEnded(true)
	if got, _ := m.Snapshot(context.Background(), time.Unix(3000, 0)); len(got) != 1 {
		t.Fatalf("ended session hidden with ShowEnded: %d rows", len(got))
	}
}

// TestProviderErrorDoesNotBlankTable checks that one failing provider does
// not suppress a healthy one's rows.
func TestProviderErrorDoesNotBlankTable(t *testing.T) {
	good := &fakeProvider{name: "good", agents: []*agent.Agent{newAgent("cli")}}
	bad := &fakeProvider{name: "bad", err: context.DeadlineExceeded}

	m := New(Config{}, []provider.Provider{bad, good}, nil)
	got, errs := m.Snapshot(context.Background(), time.Unix(3000, 0))
	if len(got) != 1 {
		t.Fatalf("got %d agents, want 1 from the healthy provider", len(got))
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1 from the failing provider", len(errs))
	}
}

func approx(got, want, tol float64) bool {
	d := got - want
	return d < tol && d > -tol
}
