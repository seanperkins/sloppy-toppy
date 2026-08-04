// Package provider defines the adapter contract that every agent runtime
// plugs into, plus the separate contract for remote spend sources.
package provider

import (
	"context"
	"time"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
)

// Provider is a source of live agent sessions backed by local state that
// can be polled cheaply and repeatedly.
//
// Implementations must be safe to call concurrently with other providers
// and must never write to the runtime's own state. Poll should return
// whatever it could read rather than failing wholesale: a single unreadable
// session file should not blank the table.
type Provider interface {
	// Name is the short adapter identity used in the agent label,
	// e.g. "hermes", "claude", "codex".
	Name() string

	// Poll returns every session this provider can currently see,
	// including ended ones — filtering is the monitor's job. now is
	// passed in so all providers in a poll share one clock, which keeps
	// derived rates consistent.
	Poll(ctx context.Context, now time.Time) ([]*agent.Agent, error)
}

// Spend is a remote billing or usage source, such as OpenRouter.
//
// It is deliberately NOT a Provider. Remote billing APIs have no per-session
// heartbeat, no tool calls and no context window, so forcing them into agent
// rows would leave most columns meaningless. They also cannot be polled at
// the table's refresh rate without burning rate limit, hence the separate
// Interval.
type Spend interface {
	// Name identifies the spend source, e.g. "openrouter".
	Name() string

	// Interval is how often this source may be polled. The monitor honours
	// it independently of the agent refresh rate.
	Interval() time.Duration

	// Fetch retrieves current balance and burn. Implementations should
	// return a nil Snapshot with a nil error when the source is simply not
	// configured, so an unconfigured source stays silent instead of
	// reporting a permanent failure.
	Fetch(ctx context.Context) (*Snapshot, error)
}

// Snapshot is a point-in-time reading from a remote spend source.
type Snapshot struct {
	Source string

	// CreditsRemaining is the balance left, when the API reports one.
	CreditsRemaining float64
	HasCredits       bool

	// SpentTotal is lifetime or period spend, as reported.
	SpentTotal float64

	// CostRate is $/hour, derived by diffing SpentTotal between fetches.
	CostRate float64

	// Requests is the request count for the reported period.
	Requests int64

	FetchedAt time.Time

	// Err records a fetch failure so the UI can show the source as
	// degraded rather than silently reporting stale numbers as current.
	Err error
}
