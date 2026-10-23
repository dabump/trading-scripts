package newssentiment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/screener"
)

func TestBuildPromptIncludesSymbolAndHeadlines(t *testing.T) {
	prompt := BuildPrompt("QTEX", []string{"QTEX wins major contract", "QTEX beats on revenue"})
	for _, want := range []string{"QTEX", "QTEX wins major contract", "QTEX beats on revenue", "POSITIVE", "NEGATIVE", "NEUTRAL"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseSentiment(t *testing.T) {
	cases := map[string]string{
		"POSITIVE":                Positive,
		"positive":                Positive,
		"  Positive\n":            Positive,
		"NEGATIVE":                Negative,
		"NEUTRAL":                 Neutral,
		"I think this is unclear": Neutral,
		"":                        Neutral,
	}
	for in, want := range cases {
		if got := parseSentiment(in); got != want {
			t.Errorf("parseSentiment(%q) = %q, want %q", in, got, want)
		}
	}
}

func qualifyingEval(symbol string, newsPass bool) domain.Evaluation {
	return domain.Evaluation{
		Symbol: symbol,
		Criteria: []domain.Criterion{
			{Name: screener.CriterionNews, Pass: newsPass, Display: "1 today"},
			{Name: screener.CriterionMove, Pass: true, Display: "+12.0%"},
			{Name: screener.CriterionVolume, Pass: true, Display: "6.0x"},
		},
	}
}

// fakeStore stands in for the database, mirroring tradereview's fakeStore: a map
// of per-date evaluations and a map of what has already been saved, so the
// "already analyzed" check can be asserted the same way the real NOT EXISTS-style
// query behaves.
type fakeStore struct {
	evals    map[string][]domain.Evaluation
	saved    map[string]map[string]string // date -> symbol -> sentiment
	loadErrs int
}

func (f *fakeStore) LatestScreenSnapshot(date string) ([]domain.Evaluation, time.Time, error) {
	return f.evals[date], time.Time{}, nil
}

func (f *fakeStore) AnalyzedNewsSentiment(date string) (map[string]bool, error) {
	out := map[string]bool{}
	for sym := range f.saved[date] {
		out[sym] = true
	}
	return out, nil
}

func (f *fakeStore) SaveNewsSentiment(date, symbol, sentiment string) error {
	if f.saved == nil {
		f.saved = map[string]map[string]string{}
	}
	if f.saved[date] == nil {
		f.saved[date] = map[string]string{}
	}
	f.saved[date][symbol] = sentiment
	return nil
}

// fakeNews stands in for the broker's NewsHeadlines.
type fakeNews struct {
	headlines map[string][]string
	err       error
}

func (f *fakeNews) NewsHeadlines(_ context.Context, symbol string, _ time.Time) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.headlines[symbol], nil
}

func newWorker(store *fakeStore, news *fakeNews, analyze Analyze) *Worker {
	return &Worker{
		Store:    store,
		News:     news,
		Analyze:  analyze,
		LoggedIn: func() bool { return true },
		Now:      func() time.Time { return time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC) },
	}
}

func TestSweepAnalyzesOnlySymbolsWithANewsCatalyst(t *testing.T) {
	date := "2026-10-24"
	store := &fakeStore{evals: map[string][]domain.Evaluation{
		date: {qualifyingEval("WITHNEWS", true), qualifyingEval("NONEWS", false)},
	}}
	news := &fakeNews{headlines: map[string][]string{
		"WITHNEWS": {"WITHNEWS announces big deal"},
		"NONEWS":   {"should never be fetched for this one, but harmless if it is"},
	}}
	var analyzed []string
	w := newWorker(store, news, func(ctx context.Context, prompt string) (string, error) {
		analyzed = append(analyzed, prompt)
		return "POSITIVE", nil
	})

	w.sweep(context.Background())

	if len(analyzed) != 1 {
		t.Fatalf("analyzed %d prompts, want 1", len(analyzed))
	}
	if !strings.Contains(analyzed[0], "WITHNEWS") {
		t.Errorf("analyzed the wrong symbol: %s", analyzed[0])
	}
	if got := store.saved[date]["WITHNEWS"]; got != Positive {
		t.Errorf("saved sentiment = %q, want POSITIVE", got)
	}
	if _, ok := store.saved[date]["NONEWS"]; ok {
		t.Error("a symbol that failed the news criterion should never be saved")
	}
}

func TestSweepSkipsASymbolAlreadyAnalyzedToday(t *testing.T) {
	date := "2026-10-24"
	store := &fakeStore{
		evals: map[string][]domain.Evaluation{date: {qualifyingEval("QTEX", true)}},
		saved: map[string]map[string]string{date: {"QTEX": Positive}},
	}
	news := &fakeNews{headlines: map[string][]string{"QTEX": {"QTEX headline"}}}
	calls := 0
	w := newWorker(store, news, func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "NEGATIVE", nil
	})

	w.sweep(context.Background())

	if calls != 0 {
		t.Errorf("Claude was called %d times for an already-analyzed symbol, want 0", calls)
	}
	if got := store.saved[date]["QTEX"]; got != Positive {
		t.Errorf("existing sentiment was overwritten: %q", got)
	}
}

func TestSweepDoesNothingWhenNotLoggedIn(t *testing.T) {
	date := "2026-10-24"
	store := &fakeStore{evals: map[string][]domain.Evaluation{date: {qualifyingEval("QTEX", true)}}}
	news := &fakeNews{headlines: map[string][]string{"QTEX": {"QTEX headline"}}}
	calls := 0
	w := newWorker(store, news, func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "POSITIVE", nil
	})
	w.LoggedIn = func() bool { return false }

	w.sweep(context.Background())

	if calls != 0 {
		t.Error("Claude should not be called while logged out")
	}
	if len(store.saved) != 0 {
		t.Error("nothing should be recorded while logged out, so a later sweep retries once logged in")
	}
}

func TestSweepDoesNotSpendAnAnalysisWhenHeadlinesAreUnavailable(t *testing.T) {
	date := "2026-10-24"
	store := &fakeStore{evals: map[string][]domain.Evaluation{date: {qualifyingEval("QTEX", true)}}}
	news := &fakeNews{err: errors.New("news endpoint down")}
	calls := 0
	w := newWorker(store, news, func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "POSITIVE", nil
	})

	w.sweep(context.Background())

	if calls != 0 {
		t.Error("Claude should not be called when headlines could not be fetched")
	}
	if len(store.saved) != 0 {
		t.Error("an unavailable headline fetch must not spend the day's analysis")
	}
}

func TestSweepStillSpendsTheDailyAnalysisWhenClaudeFails(t *testing.T) {
	date := "2026-10-24"
	store := &fakeStore{evals: map[string][]domain.Evaluation{date: {qualifyingEval("QTEX", true)}}}
	news := &fakeNews{headlines: map[string][]string{"QTEX": {"QTEX headline"}}}
	calls := 0
	w := newWorker(store, news, func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "", errors.New("claude: not logged in")
	})

	w.sweep(context.Background())

	if calls != 1 {
		t.Fatalf("Claude was called %d times, want 1", calls)
	}
	if got, ok := store.saved[date]["QTEX"]; !ok || got != Neutral {
		t.Errorf("a failed analysis should still be recorded (as neutral) so it is not retried; got %q, ok=%v", got, ok)
	}

	// A second sweep must not retry it.
	w.sweep(context.Background())
	if calls != 1 {
		t.Errorf("sweep re-analyzed a symbol whose daily analysis already ran")
	}
}

func TestSweepStopsAtBatchSize(t *testing.T) {
	date := "2026-10-24"
	store := &fakeStore{evals: map[string][]domain.Evaluation{
		date: {qualifyingEval("A", true), qualifyingEval("B", true), qualifyingEval("C", true)},
	}}
	news := &fakeNews{headlines: map[string][]string{
		"A": {"A headline"}, "B": {"B headline"}, "C": {"C headline"},
	}}
	w := newWorker(store, news, func(ctx context.Context, prompt string) (string, error) {
		return "NEUTRAL", nil
	})
	w.BatchSize = 2

	w.sweep(context.Background())

	if len(store.saved[date]) != 2 {
		t.Errorf("analyzed %d symbols in one sweep, want 2 (the batch size)", len(store.saved[date]))
	}
}

func TestRunCommandCapturesStdoutAndFeedsStdin(t *testing.T) {
	out, err := runCommand(context.Background(), "sh",
		[]string{"-c", "cat; echo done"}, "the prompt text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the prompt text") || !strings.Contains(out, "done") {
		t.Errorf("output = %q", out)
	}
}

func TestRunCommandReturnsStderrOnFailure(t *testing.T) {
	_, err := runCommand(context.Background(), "sh",
		[]string{"-c", "echo nope 1>&2; exit 1"}, "")
	if err == nil {
		t.Fatal("expected an error for a nonzero exit")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want it to carry stderr", err)
	}
}
