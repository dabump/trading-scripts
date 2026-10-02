// Package audit records the decisions and actions the agent takes, as an
// append-only JSON-lines trail on disk.
//
// This is deliberately separate from the operational log. Operational logging is
// for watching the daemon work; this is for answering "why did it buy that, and
// what did it know at the time?" weeks later, when the process output is long gone.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kind enumerates the auditable events. Only state changes and actions appear here:
// the screen runs every minute, and recording each evaluation would bury the events
// that actually matter under hundreds of identical rows.
type Kind string

const (
	AgentStarted   Kind = "AGENT_STARTED"
	SentimentRead  Kind = "SENTIMENT_READING"
	GateResolved   Kind = "GATE_RESOLVED"
	TradingHalted  Kind = "TRADING_HALTED"
	PositionOpened Kind = "POSITION_OPENED"
	// PositionScaledOut is a partial sale at the first profit target. Distinct from
	// PositionClosed because the position is still open afterwards, and an end-of-day
	// summary that counted it as a close would double-count the trade.
	PositionScaledOut Kind = "POSITION_SCALED_OUT"
	PositionClosed    Kind = "POSITION_CLOSED"
	EntrySkipped      Kind = "ENTRY_SKIPPED"
	// OrderNotFilled is an exit order that sold nothing or only part of what it
	// asked for within the fill wait. Whatever did not sell is still held, and the
	// exit rule is tried again on the next tick.
	OrderNotFilled Kind = "ORDER_NOT_FILLED"
	Reconciled     Kind = "RECONCILED"
	Fault          Kind = "FAULT"
)

// Event is one audited decision or action.
type Event struct {
	At          time.Time      `json:"at"`
	SessionDate string         `json:"session_date"`
	Kind        Kind           `json:"kind"`
	Symbol      string         `json:"symbol,omitempty"`
	Summary     string         `json:"summary"`
	Detail      map[string]any `json:"detail,omitempty"`
}

// Recorder is the interface the engine depends on, so tests can capture events
// without touching a filesystem.
type Recorder interface {
	Record(Event) error
}

// Discard drops events. It exists so a nil recorder can never panic a running
// daemon mid-session.
type Discard struct{}

func (Discard) Record(Event) error { return nil }

// Memory collects events in order, for tests and assertions.
type Memory struct {
	mu     sync.Mutex
	events []Event
}

func (m *Memory) Record(ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	return nil
}

// Events returns a copy of everything recorded so far.
func (m *Memory) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}

// Kinds returns just the event kinds, in order — convenient for asserting a
// sequence without matching every field.
func (m *Memory) Kinds() []Kind {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Kind, 0, len(m.events))
	for _, ev := range m.events {
		out = append(out, ev.Kind)
	}
	return out
}

// Logger appends events to a per-day file under a directory.
type Logger struct {
	mu  sync.Mutex
	dir string

	file *os.File
	// day is the session date the open file belongs to. Splitting per day gives
	// rotation for free and partitions the trail the same way the sessions are,
	// which is how anyone reading it back would want it.
	day string
}

// Open prepares the directory. Files are created lazily, on the first event of each
// day, so starting the daemon never leaves an empty file behind.
func Open(dir string) (*Logger, error) {
	if dir == "" {
		return nil, fmt.Errorf("audit directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	return &Logger{dir: dir}, nil
}

// Record appends one event and flushes it to disk before returning.
//
// The sync is deliberate. An audit trail that loses the last few entries in a crash
// fails at exactly the moment it is most needed, and at a few dozen events a day the
// cost is irrelevant.
func (l *Logger) Record(ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode audit event: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.rotateLocked(ev.SessionDate); err != nil {
		return err
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync audit log: %w", err)
	}
	return nil
}

func (l *Logger) rotateLocked(day string) error {
	if day == "" {
		day = "unknown"
	}
	if l.file != nil && l.day == day {
		return nil
	}
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}

	// O_APPEND only: an audit trail is never rewritten, and appending means a
	// restart continues the day's file rather than truncating it.
	path := filepath.Join(l.dir, fmt.Sprintf("audit-%s.jsonl", day))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	l.file, l.day = f, day
	return nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Path is where events for the given session date are written.
func (l *Logger) Path(day string) string {
	return filepath.Join(l.dir, fmt.Sprintf("audit-%s.jsonl", day))
}
