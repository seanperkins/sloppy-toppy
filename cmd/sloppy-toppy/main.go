// Command sloppy-toppy is top for your AI agents: a live monitor of token
// burn, context fill, cost, and stuck-agent detection across agent runtimes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/seanperkins/sloppy-toppy/internal/agent"
	"github.com/seanperkins/sloppy-toppy/internal/cli"
	"github.com/seanperkins/sloppy-toppy/internal/monitor"
	"github.com/seanperkins/sloppy-toppy/internal/pricing"
	"github.com/seanperkins/sloppy-toppy/internal/provider"
	"github.com/seanperkins/sloppy-toppy/internal/provider/claudecode"
	"github.com/seanperkins/sloppy-toppy/internal/provider/codex"
	"github.com/seanperkins/sloppy-toppy/internal/provider/hermes"
	"github.com/seanperkins/sloppy-toppy/internal/provider/openrouter"
	"github.com/seanperkins/sloppy-toppy/internal/tui"
)

// version is stamped at build time by goreleaser via -ldflags.
var version = "dev"

type options struct {
	once       bool
	jsonOut    bool
	sort       string
	showEnded  bool
	refresh    time.Duration
	activeIdle time.Duration
	stuckIdle  time.Duration

	hermesBase string
	claudeBase string
	codexBase  string

	providers   string
	pricingFile string
	noSpend     bool
	showVersion bool

	lookback    time.Duration
	assumeEnded time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sloppy-toppy:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	var o options
	fs := flag.NewFlagSet("sloppy-toppy", flag.ContinueOnError)
	fs.BoolVar(&o.once, "once", false, "print one snapshot and exit")
	fs.BoolVar(&o.jsonOut, "json", false, "JSON lines output (implies --once)")
	fs.StringVar(&o.sort, "sort", string(agent.SortState), "sort column: state|cost|tokens|ctx")
	fs.BoolVar(&o.showEnded, "a", false, "include finished sessions")
	fs.BoolVar(&o.showEnded, "show-ended", false, "include finished sessions")
	fs.DurationVar(&o.refresh, "refresh", monitor.DefaultRefresh, "TUI refresh interval")
	fs.DurationVar(&o.activeIdle, "active-idle", monitor.DefaultActiveIdle, "idle time still counted as ACTIVE")
	fs.DurationVar(&o.stuckIdle, "stuck-idle", monitor.DefaultStuckIdle, "idle time before a non-interactive agent is STUCK")
	fs.StringVar(&o.hermesBase, "hermes-base", hermes.DefaultBase, "Hermes home directory")
	fs.StringVar(&o.claudeBase, "claude-base", claudecode.DefaultBase, "Claude Code home directory")
	fs.StringVar(&o.codexBase, "codex-base", codex.DefaultBase, "Codex home directory")
	fs.DurationVar(&o.lookback, "lookback", claudecode.DefaultLookback,
		"how far back a transcript may have been touched to count as a session (file-based providers)")
	fs.DurationVar(&o.assumeEnded, "assume-ended-after", claudecode.DefaultAssumeEndedAfter,
		"quiet time after which a session with no end marker is treated as finished")
	fs.StringVar(&o.providers, "providers", "hermes,claude,codex", "comma-separated providers to poll")
	fs.StringVar(&o.pricingFile, "pricing", "", "path to a pricing override file")
	fs.BoolVar(&o.noSpend, "no-spend", false,
		"do not contact remote spend APIs (OpenRouter) even if a key is set")
	fs.BoolVar(&o.showVersion, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "sloppy-toppy — top for your AI agents\n\nUsage:\n  sloppy-toppy [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}

	if o.showVersion {
		fmt.Println("sloppy-toppy", version)
		return nil
	}
	if !agent.ValidSortMode(o.sort) {
		return fmt.Errorf("invalid --sort %q: want state, cost, tokens, or ctx", o.sort)
	}
	// A zero or negative refresh would spin the TUI's ticker as fast as the
	// scheduler allows.
	if o.refresh <= 0 {
		return fmt.Errorf("--refresh must be positive, got %s", o.refresh)
	}
	if o.activeIdle < 0 || o.stuckIdle < 0 {
		return fmt.Errorf("--active-idle and --stuck-idle must not be negative")
	}

	if err := pricing.LoadOverrides(o.pricingFile); err != nil {
		return fmt.Errorf("loading pricing overrides: %w", err)
	}

	providers, err := buildProviders(o)
	if err != nil {
		return err
	}
	// Reaching an external billing API is a network egress this binary should
	// not perform just because some other tool happened to export the key.
	var spends []provider.Spend
	if !o.noSpend {
		spends = append(spends, openrouter.New())
	}

	mon := monitor.New(monitor.Config{
		ActiveIdle: o.activeIdle,
		StuckIdle:  o.stuckIdle,
		ShowEnded:  o.showEnded,
	}, providers, spends)

	// Batch mode when asked for it, or whenever stdout is not a terminal, so
	// piping into a file behaves instead of failing on a missing TTY.
	batch := o.once || o.jsonOut || !term.IsTerminal(int(os.Stdout.Fd()))
	if batch {
		return runOnce(mon, o)
	}

	// The TUI is constructed only on this branch, so the cron path above
	// never pays for terminal setup.
	return tui.Run(tui.Options{
		Monitor: mon,
		Sort:    agent.SortMode(o.sort),
		Refresh: o.refresh,
		Version: version,
	})
}

func buildProviders(o options) ([]provider.Provider, error) {
	var out []provider.Provider
	for _, name := range splitList(o.providers) {
		switch name {
		case "hermes":
			out = append(out, hermes.New(o.hermesBase))
		case "claude", "claude-code", "claudecode":
			p := claudecode.New(o.claudeBase)
			p.Lookback, p.AssumeEndedAfter = o.lookback, o.assumeEnded
			out = append(out, p)
		case "codex":
			p := codex.New(o.codexBase)
			p.Lookback, p.AssumeEndedAfter = o.lookback, o.assumeEnded
			out = append(out, p)
		case "":
		default:
			return nil, fmt.Errorf("unknown provider %q", name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no providers selected")
	}
	return out, nil
}

func runOnce(mon *monitor.Monitor, o options) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now()
	agents, errs := mon.Snapshot(ctx, now)
	// Sorting applies to both output modes: --json previously emitted rows
	// in whatever order the database returned them, ignoring --sort.
	agent.Sort(agents, agent.SortMode(o.sort))

	if o.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for _, a := range agents {
			if err := enc.Encode(cli.ToJSON(a, now)); err != nil {
				return err
			}
		}
	} else {
		if len(agents) == 0 {
			fmt.Println("no live agent sessions found")
		} else {
			fmt.Println(cli.RenderTable(agents, now))
		}
		if spend := cli.RenderSpend(mon.SpendSnapshots(ctx, now)); spend != "" {
			fmt.Println()
			fmt.Println(spend)
		}
	}

	// Provider failures go to stderr so they never corrupt the JSON stream
	// or the table on stdout.
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	// If every provider failed we have no view at all. Exiting 0 here would be
	// indistinguishable from "no sessions running" to a cron job or a script,
	// which is the one consumer that cannot see the warnings above.
	if len(errs) > 0 && len(errs) == mon.ProviderCount() {
		return fmt.Errorf("all %d providers failed", len(errs))
	}
	return nil
}

// splitList parses a comma-separated flag value, trimming spaces and
// dropping empties so "hermes, codex," behaves.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(strings.ToLower(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
