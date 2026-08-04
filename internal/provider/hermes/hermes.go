// Package hermes reads agent sessions from Hermes state databases.
//
// Schema verified against a live install (2026-08-04). The `sessions` table
// holds cumulative token counters, both an estimated and an actual cost, and
// a per-session `source` column ("cli", "telegram", "cron") that is the real
// signal for whether a long idle means "wedged" or "abandoned chat thread".
//
// All access is strictly read-only: the DSN is opened with mode=ro and the
// adapter never issues a write.
package hermes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so binaries stay static

	"github.com/seanperkins/sloppy-toppy/internal/agent"
)

// DefaultBase is where Hermes keeps its state unless told otherwise.
const DefaultBase = "~/.hermes"

// DefaultCtxWindow is used when a model is absent from the context cache.
const DefaultCtxWindow = 128_000

// Provider polls every live Hermes install under a base directory.
type Provider struct {
	// Base is the Hermes home (~/.hermes), containing the default install
	// and a profiles/ directory of additional ones.
	Base string

	// DefaultCtx is the assumed context window for uncached models.
	DefaultCtx int64
}

// New builds a Hermes provider rooted at base.
func New(base string) *Provider {
	if base == "" {
		base = DefaultBase
	}
	return &Provider{Base: base, DefaultCtx: DefaultCtxWindow}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "hermes" }

// Install is one discovered Hermes state directory with a live gateway.
type Install struct {
	Home       string // profile dir, or the base for the default install
	StateDB    string
	Profile    string
	GatewayPID int
}

// pidFile is the JSON Hermes writes to gateway.pid.
type pidFile struct {
	PID  int    `json:"pid"`
	Kind string `json:"kind"`
	// StartTime is the gateway's process start time. Comparing it guards
	// against PID reuse: a recycled PID belonging to an unrelated process
	// would otherwise read as a live gateway.
	StartTime int64  `json:"start_time"`
	Home      string `json:"hermes_home"`
}

// expandHome resolves a leading ~ against the user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// processAlive reports whether pid names a running process.
//
// This replaces the previous /proc/<pid> existence check, which silently
// reported every process as dead on macOS and the BSDs — making the whole
// tool a no-op there. Signal 0 performs the permission and existence checks
// without delivering anything, and works on every Unix.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists but belongs to another user, which
	// still counts as alive. Only ESRCH means genuinely gone.
	return errors.Is(err, syscall.EPERM)
}

func readPIDFile(path string) (pidFile, error) {
	var pf pidFile
	data, err := os.ReadFile(path)
	if err != nil {
		return pf, err
	}
	if err := json.Unmarshal(data, &pf); err != nil {
		return pf, err
	}
	return pf, nil
}

// Discover returns every Hermes install that has a state DB and a live
// gateway. Installs whose gateway has exited are skipped: their sessions are
// history, not something currently running.
func (p *Provider) Discover() []Install {
	base := expandHome(p.Base)
	var installs []Install

	// The default install lives directly under the base.
	if inst, ok := loadInstall(base, "default"); ok {
		installs = append(installs, inst)
	}

	// Additional profiles live under profiles/<name>/.
	entries, err := os.ReadDir(filepath.Join(base, "profiles"))
	if err != nil {
		return installs
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, "profiles", e.Name())
		if inst, ok := loadInstall(dir, e.Name()); ok {
			installs = append(installs, inst)
		}
	}
	return installs
}

func loadInstall(dir, profile string) (Install, bool) {
	db := filepath.Join(dir, "state.db")
	if _, err := os.Stat(db); err != nil {
		return Install{}, false
	}
	pf, err := readPIDFile(filepath.Join(dir, "gateway.pid"))
	if err != nil || !processAlive(pf.PID) {
		return Install{}, false
	}
	return Install{
		Home:       dir,
		StateDB:    db,
		Profile:    profile,
		GatewayPID: pf.PID,
	}, true
}

// sessionCols is the projection read from the sessions table. `source` is
// included and — unlike the previous implementation — actually used.
const sessionCols = `
	id, source, model, started_at, ended_at, end_reason,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	reasoning_tokens, estimated_cost_usd, actual_cost_usd,
	last_activity_at, last_activity_description, title, cwd, git_branch,
	api_call_count, tool_call_count`

// lastToolSQL finds each session's most recent tool call. messages.id is an
// INTEGER PRIMARY KEY on the live schema, so MAX(id) is a valid proxy for
// "most recent".
const lastToolSQL = `
	SELECT session_id, tool_name FROM messages
	WHERE tool_name IS NOT NULL AND tool_name != ''
	  AND id IN (
	      SELECT MAX(id) FROM messages
	      WHERE tool_name IS NOT NULL AND tool_name != ''
	      GROUP BY session_id
	  )`

// readOnlyDSN builds a sqlite URI for path with read-only mode.
//
// The path is escaped rather than interpolated: a base directory containing
// '?' or '#' would otherwise produce a malformed URI and a confusing open
// error.
func readOnlyDSN(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	q.Set("mode", "ro")
	// Immutable=0 keeps us honest about a DB the gateway is actively writing.
	q.Set("_pragma", "busy_timeout(2000)")
	return u.String() + "?" + q.Encode()
}

// Poll implements provider.Provider, reading every live install.
func (p *Provider) Poll(ctx context.Context, now time.Time) ([]*agent.Agent, error) {
	var out []*agent.Agent
	var firstErr error
	for _, inst := range p.Discover() {
		agents, err := p.readInstall(ctx, inst, now)
		if err != nil {
			// One unreadable install must not blank the whole table.
			if firstErr == nil {
				firstErr = fmt.Errorf("hermes %s: %w", inst.Profile, err)
			}
			continue
		}
		out = append(out, agents...)
	}
	return out, firstErr
}

func (p *Provider) readInstall(ctx context.Context, inst Install, now time.Time) ([]*agent.Agent, error) {
	db, err := sql.Open("sqlite", readOnlyDSN(inst.StateDB))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT "+sessionCols+
		" FROM sessions WHERE model IS NOT NULL AND started_at IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ctxCache := loadContextCache(inst.Home)
	lastTools := readLastTools(ctx, db)
	defaultCtx := p.DefaultCtx
	if defaultCtx <= 0 {
		defaultCtx = DefaultCtxWindow
	}

	var agents []*agent.Agent
	for rows.Next() {
		var (
			id, source, model                    sql.NullString
			startedAt, endedAt, lastActivityAt   sql.NullFloat64
			endReason, lastDesc, title           sql.NullString
			cwd, gitBranch                       sql.NullString
			inTok, outTok, cacheRead, cacheWrite sql.NullInt64
			reasoning, apiCalls, toolCalls       sql.NullInt64
			estCost, actualCost                  sql.NullFloat64
		)
		if err := rows.Scan(
			&id, &source, &model, &startedAt, &endedAt, &endReason,
			&inTok, &outTok, &cacheRead, &cacheWrite,
			&reasoning, &estCost, &actualCost,
			&lastActivityAt, &lastDesc, &title, &cwd, &gitBranch,
			&apiCalls, &toolCalls,
		); err != nil {
			return nil, err
		}

		a := &agent.Agent{
			Provider:  "hermes",
			Instance:  inst.Profile,
			SessionID: id.String,
			// The session's own source drives STUCK classification. This is
			// the field the previous implementation selected and discarded,
			// leaving the heuristic keyed to the profile name instead.
			Origin:           source.String,
			Title:            title.String,
			Model:            model.String,
			StartedAt:        epoch(startedAt),
			LastActivityAt:   epoch(lastActivityAt),
			EndedAt:          epoch(endedAt),
			EndReason:        endReason.String,
			InputTokens:      inTok.Int64,
			OutputTokens:     outTok.Int64,
			CacheReadTokens:  cacheRead.Int64,
			CacheWriteTokens: cacheWrite.Int64,
			ReasoningTokens:  reasoning.Int64,
			LastTool:         lastTools[id.String],
			LastActionDesc:   lastDesc.String,
			CWD:              cwd.String,
			GitBranch:        gitBranch.String,
			APICallCount:     apiCalls.Int64,
			ToolCallCount:    toolCalls.Int64,
			OwnerPID:         inst.GatewayPID,
			CtxWindow:        modelCtxWindow(model.String, ctxCache, defaultCtx),
		}

		// Context fill is a lower bound here: cumulative cache_read_tokens
		// accumulate across every API call and would blow past 100%, so they
		// are excluded. Unlike Claude Code and Codex, Hermes gives us no way
		// to see the true current prefix — hence CtxAccurate stays false.
		a.CtxUsed = a.InputTokens + a.OutputTokens + a.ReasoningTokens
		a.CtxAccurate = false

		// Prefer the settled figure over the running estimate.
		switch {
		case actualCost.Valid && actualCost.Float64 > 0:
			a.CostUSD, a.CostSource = actualCost.Float64, agent.CostReported
		case estCost.Valid:
			a.CostUSD, a.CostSource = estCost.Float64, agent.CostReported
		default:
			a.CostSource = agent.CostUnknown
		}

		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func readLastTools(ctx context.Context, db *sql.DB) map[string]string {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, lastToolSQL)
	if err != nil {
		// The messages table is an optimisation, not a requirement.
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sid, tool sql.NullString
		if err := rows.Scan(&sid, &tool); err != nil {
			return out
		}
		out[sid.String] = tool.String
	}
	return out
}

// epoch converts a nullable float epoch to a time, treating null and zero as
// "unknown" rather than as 1970.
func epoch(v sql.NullFloat64) time.Time {
	if !v.Valid || v.Float64 <= 0 {
		return time.Time{}
	}
	sec, frac := int64(v.Float64), v.Float64-float64(int64(v.Float64))
	return time.Unix(sec, int64(frac*1e9))
}

// ---------------------------------------------------------------------------
// context window cache
// ---------------------------------------------------------------------------

// loadContextCache parses Hermes's context_length_cache.yaml, a flat
// `model@base_url: N` map, without taking a YAML dependency.
func loadContextCache(home string) map[string]int64 {
	cache := map[string]int64{}
	data, err := os.ReadFile(filepath.Join(home, "context_length_cache.yaml"))
	if err != nil {
		return cache
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip comments: without this, a leading "# cached at: 1234" is
		// happily parsed as a model named "# cached at".
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Keys contain colons (model@https://host/v1), so split on the last
		// one and require the value to be a bare integer.
		i := strings.LastIndex(trimmed, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:i])
		val := strings.TrimSpace(trimmed[i+1:])
		if key == "" || val == "" {
			continue
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		cache[key] = n
	}
	return cache
}

// modelCtxWindow looks up a model's window, tolerating the @base_url suffix
// that cache keys carry.
func modelCtxWindow(model string, cache map[string]int64, fallback int64) int64 {
	if model == "" {
		return fallback
	}
	if n, ok := cache[model]; ok {
		return n
	}
	prefix := model + "@"
	for k, v := range cache {
		if strings.HasPrefix(k, prefix) {
			return v
		}
	}
	return fallback
}
