package agent

import (
	"testing"
	"time"
)

func TestCtxPctClamped(t *testing.T) {
	tests := []struct {
		name string
		used int64
		win  int64
		want float64
	}{
		{name: "half full", used: 500, win: 1000, want: 50},
		{name: "over-full clamps to 100", used: 10_000_000, win: 128_000, want: 100},
		{name: "no window reports zero", used: 500, win: 0, want: 0},
		{name: "negative window reports zero", used: 500, win: -1, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{CtxUsed: tc.used, CtxWindow: tc.win}
			if got := a.CtxPct(); got != tc.want {
				t.Fatalf("ctx pct = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsInteractiveUsesOrigin(t *testing.T) {
	// Case must not matter, and the instance name must be irrelevant.
	interactive := &Agent{Origin: "Telegram", Instance: "default"}
	if !interactive.IsInteractive() {
		t.Fatal("telegram origin not treated as interactive")
	}
	autonomous := &Agent{Origin: "cron", Instance: "slack"}
	if autonomous.IsInteractive() {
		t.Fatal("a cron session under a slack-named instance counted as interactive")
	}
	if (&Agent{Origin: ""}).IsInteractive() {
		t.Fatal("an empty origin defaulted to interactive")
	}
}

func TestUptime(t *testing.T) {
	now := time.Unix(2000, 0)
	running := &Agent{StartedAt: time.Unix(1000, 0)}
	if got := running.Uptime(now); got != 1000*time.Second {
		t.Fatalf("uptime = %v, want 1000s", got)
	}
	// A finished session freezes at its end time rather than growing.
	ended := &Agent{StartedAt: time.Unix(1000, 0), EndedAt: time.Unix(1500, 0)}
	if got := ended.Uptime(now); got != 500*time.Second {
		t.Fatalf("ended uptime = %v, want 500s", got)
	}
	if got := (&Agent{}).Uptime(now); got != 0 {
		t.Fatalf("unknown start gave uptime %v, want 0", got)
	}
}

func TestKeyIsUniqueAcrossProviders(t *testing.T) {
	// Two providers can legitimately use the same session id; the key must
	// keep their rate bookkeeping separate.
	a := &Agent{Provider: "hermes", Instance: "default", SessionID: "x"}
	b := &Agent{Provider: "codex", Instance: "default", SessionID: "x"}
	if a.Key() == b.Key() {
		t.Fatalf("keys collide across providers: %q", a.Key())
	}
}

func TestSortOrdersByStateThenMetric(t *testing.T) {
	agents := []*Agent{
		{SessionID: "idle", State: StateIdle, CostRate: 100},
		{SessionID: "active-cheap", State: StateActive, CostRate: 1},
		{SessionID: "active-pricey", State: StateActive, CostRate: 50},
		{SessionID: "done", State: StateDone, CostRate: 999},
	}
	Sort(agents, SortCost)

	// State rank is always primary, so an expensive IDLE never outranks a
	// cheap ACTIVE.
	want := []string{"active-pricey", "active-cheap", "idle", "done"}
	for i, id := range want {
		if agents[i].SessionID != id {
			t.Fatalf("position %d = %q, want %q (full order: %v)",
				i, agents[i].SessionID, id, ids(agents))
		}
	}
}

// TestSortIsStableForTies keeps rows from jittering between refreshes when
// two agents compare equal.
func TestSortIsStableForTies(t *testing.T) {
	mk := func() []*Agent {
		return []*Agent{
			{Provider: "p", SessionID: "b", State: StateActive},
			{Provider: "p", SessionID: "a", State: StateActive},
		}
	}
	first, second := mk(), mk()
	Sort(first, SortState)
	Sort(second, SortState)
	for i := range first {
		if first[i].SessionID != second[i].SessionID {
			t.Fatalf("sort is not deterministic: %v vs %v", ids(first), ids(second))
		}
	}
}

func TestUnknownStateSortsLastNotPanics(t *testing.T) {
	agents := []*Agent{
		{SessionID: "future", State: State("SOMETHING_NEW")},
		{SessionID: "active", State: StateActive},
	}
	Sort(agents, SortState) // must not panic
	if agents[0].SessionID != "active" {
		t.Fatalf("unknown state did not sort last: %v", ids(agents))
	}
}

func TestValidSortMode(t *testing.T) {
	for _, ok := range []string{"state", "cost", "tokens", "ctx"} {
		if !ValidSortMode(ok) {
			t.Fatalf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "COST", "uptime"} {
		if ValidSortMode(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestFmtUptime(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: 0, want: "—"},
		{d: 45 * time.Second, want: "45s"},
		{d: 90 * time.Second, want: "1m30"},
		{d: 3*time.Hour + 5*time.Minute, want: "3h05"},
		{d: 50 * time.Hour, want: "2d02h"},
	}
	for _, tc := range tests {
		if got := FmtUptime(tc.d); got != tc.want {
			t.Fatalf("FmtUptime(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFmtTokens(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{n: 512, want: "512"},
		{n: 1400, want: "1.4k"},
		{n: 2_300_000, want: "2.3M"},
	}
	for _, tc := range tests {
		if got := FmtTokens(tc.n); got != tc.want {
			t.Fatalf("FmtTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFmtUSDKeepsSubCentCostsLegible(t *testing.T) {
	if got := FmtUSD(0.00042); got != "0.00042" {
		t.Fatalf("sub-cent cost = %q, want full precision", got)
	}
	if got := FmtUSD(12.345); got != "12.35" {
		t.Fatalf("FmtUSD(12.345) = %q, want 12.35", got)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	// Multi-byte titles must not be cut mid-character.
	got := Truncate("日本語のタイトルです", 5)
	if len([]rune(got)) != 5 {
		t.Fatalf("Truncate produced %d runes, want 5: %q", len([]rune(got)), got)
	}
	if got := Truncate("short", 20); got != "short" {
		t.Fatalf("short string was altered: %q", got)
	}
}

func TestShortModel(t *testing.T) {
	if got := (&Agent{Model: "deepseek/deepseek-v4-flash"}).ShortModel(); got != "deepseek-v4-flash" {
		t.Fatalf("ShortModel = %q", got)
	}
	if got := (&Agent{Model: ""}).ShortModel(); got != "—" {
		t.Fatalf("empty model = %q, want a dash", got)
	}
}

func TestCostSourceKnown(t *testing.T) {
	if CostUnknown.Known() {
		t.Fatal("CostUnknown reported as known")
	}
	if !CostReported.Known() || !CostEstimated.Known() {
		t.Fatal("a real cost source reported as unknown")
	}
}

func ids(agents []*Agent) []string {
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = a.SessionID
	}
	return out
}
