package tradereview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

func TestBuildPromptIncludesTradeNumbersAndDocs(t *testing.T) {
	entry := time.Date(2026, 10, 20, 9, 31, 0, 0, time.UTC)
	exit := entry.Add(12 * time.Minute)
	p := domain.Position{
		ID: 42, Symbol: "QTEX", SessionDate: "2026-10-20",
		Shares: 100, EntryPrice: 5.00, EntryTime: entry,
		StopPrice: 4.80, InitialRisk: 0.20, PeakPrice: 5.10,
		ExitPrice: 4.80, ExitTime: exit, ExitReason: domain.ExitStopLoss,
		BankedDollars: -20,
	}

	prompt := BuildPrompt(p, "THE STRATEGY DOC CONTENTS")

	for _, want := range []string{
		"QTEX", "2026-10-20", "THE STRATEGY DOC CONTENTS",
		"100 shares at $5.0000", "$4.8000", "STOP_LOSS", "-20.00",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptHandlesMissingDocs(t *testing.T) {
	prompt := BuildPrompt(domain.Position{Symbol: "X"}, "")
	if !strings.Contains(prompt, "(not available)") {
		t.Errorf("expected a placeholder for missing docs, got:\n%s", prompt)
	}
}

// fakeStore stands in for the database: one position needing review, which
// disappears once SaveTradeReview is called for it — the same "no row yet means
// pending" behavior the real store implements via NOT EXISTS.
type fakeStore struct {
	pending []domain.Position
	saved   map[int64]struct{ report, err string }
}

func (f *fakeStore) FailedTradesNeedingReview(limit int) ([]domain.Position, error) {
	var out []domain.Position
	for _, p := range f.pending {
		if _, done := f.saved[p.ID]; !done {
			out = append(out, p)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) SaveTradeReview(positionID int64, report, reviewErr string) error {
	if f.saved == nil {
		f.saved = map[int64]struct{ report, err string }{}
	}
	f.saved[positionID] = struct{ report, err string }{report, reviewErr}
	return nil
}

func TestWorkerSavesASuccessfulReview(t *testing.T) {
	store := &fakeStore{pending: []domain.Position{{ID: 1, Symbol: "ABC"}}}
	w := &Worker{
		Store: store,
		Analyze: func(ctx context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "ABC") {
				t.Errorf("prompt did not describe the trade: %s", prompt)
			}
			return "it was the stop doing its job", nil
		},
	}

	w.sweep(context.Background())

	got, ok := store.saved[1]
	if !ok {
		t.Fatal("expected position 1 to have a saved review")
	}
	if got.report != "it was the stop doing its job" || got.err != "" {
		t.Errorf("saved = %+v", got)
	}

	// A second sweep must not ask again: FailedTradesNeedingReview no longer
	// returns a position once it has a row, same as the real NOT EXISTS query.
	calls := 0
	w.Analyze = func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "", nil
	}
	w.sweep(context.Background())
	if calls != 0 {
		t.Errorf("sweep re-reviewed an already-reviewed position")
	}
}

func TestWorkerRecordsAnAnalyzeFailure(t *testing.T) {
	store := &fakeStore{pending: []domain.Position{{ID: 2, Symbol: "XYZ"}}}
	w := &Worker{
		Store: store,
		Analyze: func(ctx context.Context, prompt string) (string, error) {
			return "", errors.New("claude: not logged in")
		},
	}

	w.sweep(context.Background())

	got, ok := store.saved[2]
	if !ok {
		t.Fatal("expected position 2 to have a saved (failed) review")
	}
	if got.err == "" {
		t.Error("a failed analysis should record why")
	}
}

func TestWorkerStopsAtBatchSize(t *testing.T) {
	store := &fakeStore{pending: []domain.Position{{ID: 1}, {ID: 2}, {ID: 3}}}
	var seen []int64
	w := &Worker{
		Store:     store,
		BatchSize: 2,
		Analyze: func(ctx context.Context, prompt string) (string, error) {
			return "ok", nil
		},
	}
	// Record which ids actually got reviewed by checking what got saved.
	w.sweep(context.Background())
	for id := range store.saved {
		seen = append(seen, id)
	}
	if len(seen) != 2 {
		t.Errorf("reviewed %d positions in one sweep, want 2 (the batch size)", len(seen))
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

func TestRunCommandRespectsContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := runCommand(ctx, "sh", []string{"-c", "sleep 5"}, "")
	if err == nil {
		t.Fatal("expected the context deadline to kill a long-running command")
	}
}
