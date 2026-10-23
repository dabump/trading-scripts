// Package newssentiment asks Claude whether a screened symbol's same-day news
// catalyst reads as positive or negative, purely to put a star on the watchlist —
// it never feeds back into screening or entry. docs/architecture.md requires one
// evaluation to feed both the page and the buy decision; this worker sits entirely
// outside that path, the same way internal/tradereview does, and joins its answer
// onto the watchlist at render time instead of adding a second evaluation.
//
// It runs on its own ticker, apart from engine.Tick: a CLI call can take tens of
// seconds, and nothing about a cosmetic annotation is time-critical. It depends
// only on store.Store and a couple of narrow interfaces, not on the engine, so it
// cannot affect trading even if the CLI hangs or is not logged in.
package newssentiment

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
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/screener"
)

// Sentiment verdicts. The exact strings are what parseSentiment produces and what
// the store persists and compares against, so they must stay in sync with each
// other but owe nothing to Claude's own wording beyond what BuildPrompt asks for.
const (
	Positive = "POSITIVE"
	Negative = "NEGATIVE"
	Neutral  = "NEUTRAL"
)

// Store is what the worker needs from persistence.
type Store interface {
	// LatestScreenSnapshot is the same screening pass the watchlist page reads —
	// see docs/architecture.md on why there is only one evaluation path.
	LatestScreenSnapshot(date string) ([]domain.Evaluation, time.Time, error)
	// AnalyzedNewsSentiment returns the symbols already assessed on date, so the
	// worker can tell whether this session's one allowed analysis for a symbol has
	// already been spent.
	AnalyzedNewsSentiment(date string) (map[string]bool, error)
	SaveNewsSentiment(date, symbol, sentiment string) error
}

// News is the one broker call the worker needs. A separate, narrow interface
// rather than depending on broker.MarketData, so a test can fake just this.
type News interface {
	// NewsHeadlines returns symbol's recent story headlines since the given time —
	// the actual text behind the catalyst count the screen already saw, which the
	// scan path itself never needs.
	NewsHeadlines(ctx context.Context, symbol string, since time.Time) ([]string, error)
}

// Analyze runs one prompt through Claude and returns its answer.
type Analyze func(ctx context.Context, prompt string) (string, error)

// CLIAnalyze returns an Analyze that runs the real Claude Code CLI in print mode
// (-p), reading the prompt from stdin. Every tool is disabled (--tools "") for the
// same reason tradereview.CLIAnalyze disables them: this is pure text
// classification over what the prompt already contains, and print mode has no
// human to answer a tool-permission prompt.
//
// model is passed through to --model, same rationale as tradereview.CLIAnalyze:
// the account's default model is Opus, and a one-word classification run
// unattended, repeatedly, should not default to spending it.
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

// runCommand mirrors tradereview.runCommand, including the same fix: it does not
// inherit the daemon's working directory, because running claude -p from this
// repo's root made it auto-discover this repo's own CLAUDE.md and start
// investigating the codebase instead of answering (verified live).
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

const (
	defaultInterval  = 5 * time.Minute
	defaultBatchSize = 5
)

// Worker looks at the latest screening pass for symbols whose news-catalyst
// criterion passed, and asks Claude — at most once per symbol per session — to
// classify that news.
type Worker struct {
	Store   Store
	News    News
	Analyze Analyze
	// LoggedIn reports whether the Claude CLI has an active session. When false,
	// the worker skips the sweep without calling Claude or recording anything —
	// deliberately unlike a trade review's permanent FAILED row: "not logged in
	// yet" must not stick a symbol with a stale answer for the rest of the
	// session, so nothing is spent and the next sweep simply tries again once
	// someone does log in.
	LoggedIn func() bool
	// NewsLookback is how far back a headline search reaches. It should match
	// cfg.Screening.NewsLookback, so the sentiment call sees the same stories the
	// catalyst criterion counted.
	NewsLookback time.Duration
	// Now is injected so a sweep's session date is deterministic in tests.
	Now       func() time.Time
	Interval  time.Duration
	BatchSize int
	Log       *slog.Logger
}

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
	if w.LoggedIn != nil && !w.LoggedIn() {
		return
	}

	now := w.now()
	date := scheduler.SessionDate(now)

	evals, _, err := w.Store.LatestScreenSnapshot(date)
	if err != nil {
		w.logger().Error("load screen snapshot for sentiment", "err", err)
		return
	}
	candidates := symbolsWithCatalyst(evals)
	if len(candidates) == 0 {
		return
	}

	analyzed, err := w.Store.AnalyzedNewsSentiment(date)
	if err != nil {
		w.logger().Error("load analyzed news sentiment", "err", err)
		return
	}

	batch := w.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	since := now.Add(-w.NewsLookback)
	worked := 0
	for _, symbol := range candidates {
		if analyzed[symbol] {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		w.assess(ctx, date, symbol, since)
		worked++
		if worked >= batch {
			return
		}
	}
}

// symbolsWithCatalyst returns the symbols whose news-catalyst criterion passed in
// evals, in the order the screen ranked them.
func symbolsWithCatalyst(evals []domain.Evaluation) []string {
	var out []string
	for _, e := range evals {
		for _, c := range e.Criteria {
			if c.Name == screener.CriterionNews && c.Pass {
				out = append(out, e.Symbol)
				break
			}
		}
	}
	return out
}

// assess spends one symbol's daily analysis. Fetching headlines costs nothing
// Claude-side, so an empty or unavailable result is retried next sweep rather than
// recorded; once Claude is actually asked, the day's one allowed analysis is spent
// whether or not the call succeeds — a transient CLI error retrying on every sweep
// for the rest of the session is exactly what "sparingly" rules out.
func (w *Worker) assess(ctx context.Context, date, symbol string, since time.Time) {
	headlines, err := w.News.NewsHeadlines(ctx, symbol, since)
	if err != nil {
		w.logger().Warn("news headlines unavailable; will retry next sweep", "symbol", symbol, "err", err)
		return
	}
	if len(headlines) == 0 {
		w.logger().Debug("no headline text yet for a symbol with a news catalyst; will retry next sweep", "symbol", symbol)
		return
	}

	sentiment := Neutral
	if report, err := w.Analyze(ctx, BuildPrompt(symbol, headlines)); err != nil {
		w.logger().Warn("news sentiment analysis failed", "symbol", symbol, "err", err)
	} else {
		sentiment = parseSentiment(report)
	}

	if err := w.Store.SaveNewsSentiment(date, symbol, sentiment); err != nil {
		w.logger().Error("save news sentiment", "symbol", symbol, "err", err)
		return
	}
	w.logger().Info("news sentiment assessed", "symbol", symbol, "sentiment", sentiment)
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) logger() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

// BuildPrompt assembles the sentiment request: a symbol's recent headlines and a
// strict one-word answer format, so parseSentiment never has to guess at
// free-form prose.
func BuildPrompt(symbol string, headlines []string) string {
	var b strings.Builder
	// Verified live for tradereview's equivalent prompt: without an explicit "no
	// tools" preamble, -p mode's usual coding-agent instincts kick in and it tries
	// to go "check the logs" or "look this up" instead of answering, even with
	// every tool disabled.
	b.WriteString("You have no tools, no filesystem access, and no ability to look " +
		"anything up. The headlines below are the ONLY information that exists for " +
		"this task — there is nothing else to check; answer directly from what is " +
		"given.\n\n")
	fmt.Fprintf(&b, "Below are today's news headlines for %s, a stock that just had a "+
		"large intraday price move on unusually high volume. Classify whether this "+
		"news reads as the reason behind a move that should continue (POSITIVE — a "+
		"new contract, an earnings or guidance beat, a drug approval, a takeover or "+
		"upgrade, and the like), the reason behind a move that should reverse "+
		"(NEGATIVE — a guidance cut, dilution or an offering, a lawsuit, a "+
		"delisting or downgrade, and the like), or neither (NEUTRAL — routine, "+
		"unclear, or no real catalyst in these headlines).\n\n"+
		"Answer with exactly one word — POSITIVE, NEGATIVE, or NEUTRAL — and "+
		"nothing else: no explanation, no punctuation. Answer now.\n\n", symbol)

	b.WriteString("=== HEADLINES ===\n")
	for _, h := range headlines {
		b.WriteString("- ")
		b.WriteString(h)
		b.WriteString("\n")
	}
	return b.String()
}

// parseSentiment reads Claude's answer defensively: it is asked for exactly one
// word, but anything other than a clean, confident match falls back to Neutral —
// the star is a nice-to-have, not something worth misreading free-form text for.
func parseSentiment(report string) string {
	upper := strings.ToUpper(report)
	switch {
	case strings.Contains(upper, Positive):
		return Positive
	case strings.Contains(upper, Negative):
		return Negative
	default:
		return Neutral
	}
}
