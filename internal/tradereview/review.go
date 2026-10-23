// Package tradereview asks Claude why a losing trade lost, and what — if
// anything — should change about the strategy for a trade like it to have closed
// in the green. It is fed the strategy's own documentation alongside the trade's
// numbers, so the answer is grounded in the rules actually in force rather than
// Claude's own guess at them.
//
// The worker runs on its own ticker, entirely apart from engine.Tick: one review
// can take tens of seconds (a CLI invocation, not an API call the engine already
// budgets for), and nothing about it is time-critical the way a trading decision
// is. It depends only on store.Store and an Analyze function, not on the engine,
// so it cannot affect trading even if the Claude CLI hangs or is not logged in.
package tradereview

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// Store is what the worker needs from persistence. An interface so tests can fake
// it without a real database.
type Store interface {
	FailedTradesNeedingReview(limit int) ([]domain.Position, error)
	SaveTradeReview(positionID int64, report, reviewErr string) error
}

// Analyze runs one prompt through Claude and returns its answer. A function value
// rather than a hardcoded call so a test can fake it without the real CLI, which
// the Go build stage — and most developer machines — do not have installed or
// logged in.
type Analyze func(ctx context.Context, prompt string) (string, error)

// CLIAnalyze returns an Analyze that runs the real Claude Code CLI in print mode
// (-p), reading the prompt from stdin rather than argv — a prompt here includes
// the whole strategy documentation, tens of kilobytes, comfortably inside a
// command's stdin but closer to the edge of argv limits than is worth risking.
//
// Every tool is disabled (--tools ""). This is pure text analysis over what the
// prompt already contains, not a coding task, and print mode has no human to
// answer a tool-permission prompt — leaving tools on risks a hang, not an error.
//
// model is passed through to --model. Explicit rather than left to the CLI's own
// default: the account's default model is Opus, which a background worker running
// unattended should not silently be spending on a one-off text analysis — see
// docs/decisions.md.
func CLIAnalyze(command, model string, timeout time.Duration) Analyze {
	if command == "" {
		command = "claude"
	}
	if model == "" {
		model = "sonnet"
	}
	return func(ctx context.Context, prompt string) (string, error) {
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return runCommand(ctx, command, []string{"-p", "--tools", "", "--model", model}, prompt)
	}
}

// runCommand runs command with args, writing prompt to its stdin and returning its
// trimmed stdout. Split out from CLIAnalyze so it can be tested against a fake
// script instead of the real claude binary, which the Go build stage — and most
// developer machines — do not have installed or logged in.
//
// It deliberately does not inherit the daemon's own working directory. Verified
// live: running claude -p from this very repo's root made it auto-discover this
// repo's own CLAUDE.md and start investigating the live codebase as a coding agent
// instead of answering the prompt — a one-off trade review has nothing to do with
// whatever repo the daemon happens to be started from. The container's real
// working directory (/app) has no CLAUDE.md either way, but a bare-binary or dev
// run from a repo root would otherwise hit this every time.
func runCommand(ctx context.Context, command string, args []string, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(prompt)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", command, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Worker finds closed, losing positions with no review yet and asks Claude about
// each one in turn.
type Worker struct {
	Store   Store
	Analyze Analyze
	// Docs is the strategy documentation the review is grounded in — concatenated
	// once at startup, not re-read per trade.
	Docs string
	// Interval is how often to look for newly closed losses. Defaults to 5m.
	Interval time.Duration
	// BatchSize bounds how many are worked through per sweep — reviews still run
	// strictly one at a time regardless, this just bounds how long one sweep runs
	// before the worker goes back to waiting for the next interval. Defaults to 5.
	BatchSize int
	Log       *slog.Logger
}

const (
	defaultInterval  = 5 * time.Minute
	defaultBatchSize = 5
)

// Run looks for work immediately, then on every tick of Interval, until ctx is
// cancelled.
func (w *Worker) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *Worker) sweep(ctx context.Context) {
	batch := w.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	trades, err := w.Store.FailedTradesNeedingReview(batch)
	if err != nil {
		w.logger().Error("find trades needing review", "err", err)
		return
	}
	for _, p := range trades {
		if ctx.Err() != nil {
			return
		}
		w.review(ctx, p)
	}
}

func (w *Worker) review(ctx context.Context, p domain.Position) {
	report, err := w.Analyze(ctx, BuildPrompt(p, w.Docs))
	reviewErr := ""
	if err != nil {
		w.logger().Warn("trade review failed", "symbol", p.Symbol, "position", p.ID, "err", err)
		reviewErr = err.Error()
	} else {
		w.logger().Info("trade reviewed", "symbol", p.Symbol, "position", p.ID)
	}
	if err := w.Store.SaveTradeReview(p.ID, report, reviewErr); err != nil {
		w.logger().Error("save trade review", "position", p.ID, "err", err)
	}
}

func (w *Worker) logger() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

// BuildPrompt assembles the review request: the strategy's own documentation,
// then this one trade's numbers. A pure function, so the prompt itself is testable
// without invoking Claude.
func BuildPrompt(p domain.Position, docs string) string {
	var b strings.Builder
	// Verified live: without this, -p mode defaults to its usual coding-agent
	// instincts and tries to go "check the logs" or "check the database" before
	// answering, even with every tool disabled — it just describes the check it
	// cannot perform instead of answering. Stating plainly that nothing else exists
	// to look at is what gets a direct answer instead.
	b.WriteString("You have no tools, no filesystem access, and no ability to look " +
		"anything up. The message below is the ONLY information that exists for this task " +
		"— there is no log file, no database and no other document to check, so do not " +
		"offer to look for one; answer directly from what is given.\n\n")
	b.WriteString("You are reviewing one losing trade from an automated momentum trading " +
		"strategy. Below is the strategy's own documentation, followed by this specific " +
		"trade's numbers.\n\n" +
		"Explain concretely why this trade lost money, and what — if anything — should " +
		"change about the strategy's rules (not hindsight about this one symbol) for a " +
		"trade like it to have closed in the green instead. Be specific: name the rule " +
		"and the number, and weigh the tradeoff of changing it against what the " +
		"documentation says that rule is for. If the loss looks like the strategy working " +
		"as designed — a stop doing its job, say — rather than a flaw, say that plainly " +
		"instead of inventing a fix. Answer now.\n\n")

	b.WriteString("=== STRATEGY DOCUMENTATION ===\n")
	if docs == "" {
		b.WriteString("(not available)\n")
	} else {
		b.WriteString(docs)
	}

	b.WriteString("\n\n=== THE TRADE ===\n")
	fmt.Fprintf(&b, "Symbol: %s\n", p.Symbol)
	fmt.Fprintf(&b, "Session date: %s\n", p.SessionDate)
	if p.Manual {
		b.WriteString("Entry was a manual override from the status page, not the automated signal.\n")
	}
	fmt.Fprintf(&b, "Entry: %d shares at $%.4f on %s\n",
		p.Shares, p.EntryPrice, p.EntryTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "Initial stop: $%.4f (%.4f risk per share)\n",
		p.EntryPrice-p.InitialRisk, p.InitialRisk)
	fmt.Fprintf(&b, "Stop at the time of exit: $%.4f\n", p.StopPrice)
	fmt.Fprintf(&b, "Peak price reached while held: $%.4f (%.2fR)\n",
		p.PeakPrice, p.RMultiple(p.PeakPrice))
	fmt.Fprintf(&b, "Target already hit before this exit: %v\n", p.TargetHit)
	fmt.Fprintf(&b, "Exit: %d shares at $%.4f on %s, reason %s\n",
		p.Shares, p.ExitPrice, p.ExitTime.Format(time.RFC3339), p.ExitReason)
	fmt.Fprintf(&b, "Held for: %s\n", p.ExitTime.Sub(p.EntryTime))
	fmt.Fprintf(&b, "Realized result: $%.2f, %.2f%% of capital committed, %.2fR\n",
		p.RealizedDollars(), p.RealizedPct(), p.RMultiple(p.ExitPrice))
	return b.String()
}
