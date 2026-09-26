package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordWritesOneJSONObjectPerLine(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	at := time.Date(2026, 9, 28, 14, 42, 0, 0, time.UTC)
	events := []Event{
		{At: at, SessionDate: "2026-09-28", Kind: GateResolved,
			Summary: "sentiment gate passed", Detail: map[string]any{"verdict": "PROCEED"}},
		{At: at.Add(time.Minute), SessionDate: "2026-09-28", Kind: PositionOpened,
			Symbol: "ABCD", Summary: "bought 2000 @ $5.00",
			Detail: map[string]any{"shares": 2000, "price": 5.0}},
	}
	for _, ev := range events {
		if err := l.Record(ev); err != nil {
			t.Fatal(err)
		}
	}

	got := readLines(t, l.Path("2026-09-28"))
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Kind != GateResolved || got[1].Kind != PositionOpened {
		t.Errorf("kinds = %q, %q", got[0].Kind, got[1].Kind)
	}
	if got[1].Symbol != "ABCD" || got[1].Detail["shares"] != float64(2000) {
		t.Errorf("event detail lost in round trip: %+v", got[1])
	}
	// Order must be preserved: the trail is read as a narrative.
	if !got[0].At.Before(got[1].At) {
		t.Error("events are not in chronological order")
	}
}

// A restart must continue the day's trail, never truncate it — that is the whole
// point of an append-only record.
func TestRecordAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Record(Event{At: at, SessionDate: "2026-09-28", Kind: AgentStarted, Summary: "first run"}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Record(Event{At: at.Add(time.Hour), SessionDate: "2026-09-28", Kind: AgentStarted, Summary: "after restart"}); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, second.Path("2026-09-28"))
	if len(got) != 2 {
		t.Fatalf("got %d events after reopen, want 2: the first run's record was lost", len(got))
	}
	if got[0].Summary != "first run" {
		t.Errorf("first event = %q, want it preserved", got[0].Summary)
	}
}

// Each session gets its own file, which gives rotation without any extra machinery.
func TestRecordSplitsFilesPerSession(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for _, day := range []string{"2026-09-28", "2026-09-29", "2026-09-28"} {
		if err := l.Record(Event{
			At: time.Now(), SessionDate: day, Kind: GateResolved, Summary: "gate",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(readLines(t, l.Path("2026-09-29"))); got != 1 {
		t.Errorf("29th has %d events, want 1", got)
	}
	// Returning to an earlier day must append to that day's file, not restart it.
	if got := len(readLines(t, l.Path("2026-09-28"))); got != 2 {
		t.Errorf("28th has %d events, want 2", got)
	}
}

func TestOpenCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory was not created: %v", err)
	}
	// No file until the first event, so a startup that records nothing leaves no
	// misleading empty trail.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("found %d files before any event was recorded, want 0", len(entries))
	}
}

func TestOpenRejectsEmptyDirectory(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("expected an error for an empty directory")
	}
}

// An event with no session date must still be recorded rather than dropped.
func TestRecordWithoutSessionDate(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := l.Record(Event{At: time.Now(), Kind: Fault, Summary: "no session yet"}); err != nil {
		t.Fatal(err)
	}
	if got := len(readLines(t, l.Path("unknown"))); got != 1 {
		t.Errorf("got %d events, want the undated event preserved", got)
	}
}

func TestMemoryRecorder(t *testing.T) {
	var m Memory
	m.Record(Event{Kind: AgentStarted})
	m.Record(Event{Kind: PositionOpened, Symbol: "ABCD"})

	if got := m.Kinds(); len(got) != 2 || got[0] != AgentStarted || got[1] != PositionOpened {
		t.Errorf("kinds = %v", got)
	}
	// Events must be a copy: a caller mutating the result cannot corrupt the record.
	events := m.Events()
	events[0].Kind = "TAMPERED"
	if m.Events()[0].Kind != AgentStarted {
		t.Error("Events() exposed internal state")
	}
}

func TestDiscardIsSafe(t *testing.T) {
	if err := (Discard{}).Record(Event{Kind: Fault}); err != nil {
		t.Errorf("Discard must never fail: %v", err)
	}
}
