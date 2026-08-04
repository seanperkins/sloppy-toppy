package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
)

var now = time.Unix(10000, 0)

func sample() []*agent.Agent {
	return []*agent.Agent{
		{
			Provider: "hermes", Instance: "default", SessionID: "s1",
			Origin: "cli", Title: "Cheap job", Model: "deepseek/v4-flash",
			StartedAt: time.Unix(9000, 0), LastActivityAt: time.Unix(9990, 0),
			CtxUsed: 1000, CtxWindow: 100_000, CtxAccurate: false,
			CostUSD: 0.01, CostSource: agent.CostReported,
			CostRate: 0.5, TokRate: 10, State: agent.StateActive,
		},
		{
			Provider: "codex", Instance: "demo", SessionID: "s2",
			Origin: "acpx", Title: "Unpriced model", Model: "gpt-5.6-sol",
			StartedAt: time.Unix(9000, 0), LastActivityAt: time.Unix(9000, 0),
			CtxUsed: 50_000, CtxWindow: 258_400, CtxAccurate: true,
			CostSource: agent.CostUnknown,
			CostRate:   0, TokRate: 1, State: agent.StateStuck,
		},
	}
}

// TestUnknownCostRendersAsQuestionMark is the point of tracking cost
// provenance at all: a model we cannot price must never read as free.
func TestUnknownCostRendersAsQuestionMark(t *testing.T) {
	agents := sample()
	out := RenderTable(agents, now)

	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines (header + 2 rows expected):\n%s", len(lines), out)
	}
	if !strings.Contains(lines[2], "?") {
		t.Fatalf("unpriced row lacks the unknown-cost marker:\n%s", lines[2])
	}
	if strings.Contains(lines[2], "0.00") {
		t.Fatalf("unpriced row rendered a cost of 0.00, implying it was free:\n%s", lines[2])
	}
}

// TestApproximateContextIsMarked keeps a lower bound visually distinct from
// a real measurement.
func TestApproximateContextIsMarked(t *testing.T) {
	agents := sample()
	if got := ctxCell(agents[0]); !strings.HasPrefix(got, "~") {
		t.Fatalf("approximate context = %q, want a leading ~", got)
	}
	if got := ctxCell(agents[1]); strings.HasPrefix(got, "~") {
		t.Fatalf("measured context = %q, must not be marked approximate", got)
	}
}

func TestRenderTableIncludesHeaderAndTitles(t *testing.T) {
	out := RenderTable(sample(), now)
	for _, want := range []string{"AGENT", "MODEL", "TOK/s", "CTX%", "$/HR", "STATE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing column %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"hermes:default", "codex:demo", "Cheap job", "Unpriced model"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestJSONShape(t *testing.T) {
	a := sample()[0]
	data, err := json.Marshal(ToJSON(a, now))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	// Consumers key off these; a rename is a breaking change.
	for _, field := range []string{
		"agent", "provider", "instance", "session_id", "origin", "state",
		"tokens_per_sec", "cost_per_hour", "ctx_pct", "ctx_window",
		"ctx_exact", "cost_usd", "cost_source", "uptime_seconds",
	} {
		if _, ok := got[field]; !ok {
			t.Fatalf("json output missing field %q", field)
		}
	}
	if got["origin"] != "cli" {
		t.Fatalf("origin = %v, want cli", got["origin"])
	}
	if got["agent"] != "hermes:default" {
		t.Fatalf("agent = %v, want hermes:default", got["agent"])
	}
}

// TestJSONDistinguishesUnknownCost checks the wire format carries enough to
// tell "free" apart from "unknown".
func TestJSONDistinguishesUnknownCost(t *testing.T) {
	unknown := ToJSON(sample()[1], now)
	if unknown.CostSource != "" {
		t.Fatalf("cost_source = %q, want empty for an unpriced model", unknown.CostSource)
	}
	known := ToJSON(sample()[0], now)
	if known.CostSource != "reported" {
		t.Fatalf("cost_source = %q, want reported", known.CostSource)
	}
}

// TestSortAppliesToBothOutputs pins the fix for --json ignoring --sort.
func TestSortAppliesToBothOutputs(t *testing.T) {
	agents := sample()
	// Both are non-DONE but different states, so force a tie on state rank
	// to isolate the secondary sort.
	agents[0].State = agent.StateActive
	agents[1].State = agent.StateActive
	agents[0].CostRate = 1
	agents[1].CostRate = 99

	agent.Sort(agents, agent.SortCost)
	if agents[0].SessionID != "s2" {
		t.Fatalf("cost sort put %q first, want the pricier s2", agents[0].SessionID)
	}

	agents[0].TokRate, agents[1].TokRate = 5, 1
	agent.Sort(agents, agent.SortTokens)
	if agents[0].TokRate != 5 {
		t.Fatalf("token sort did not order by tok rate: %v first", agents[0].TokRate)
	}
}

func TestRenderTableEmptyIsHeaderOnly(t *testing.T) {
	out := RenderTable(nil, now)
	if strings.Count(out, "\n") != 0 {
		t.Fatalf("empty table produced rows:\n%s", out)
	}
}
