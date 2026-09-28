package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recordingStop struct{ calls int }

func (r *recordingStop) Stop() { r.calls++ }

// The bug this replaces: returning after the first component let main's deferred store
// and audit Close run while the engine was still mid-tick. waitForShutdown must not
// return until every component has reported.
func TestWaitForShutdownWaitsForEveryComponent(t *testing.T) {
	errs := make(chan error, 2)
	var stop recordingStop

	// The web server returns immediately on cancellation; the engine takes a moment.
	errs <- http.ErrServerClosed

	returned := make(chan error, 1)
	go func() { returned <- waitForShutdown(errs, 2, &stop, time.Second, testLogger()) }()

	select {
	case <-returned:
		t.Fatal("returned after only one component reported: the caller's deferred " +
			"Close calls would run while the engine is still writing")
	case <-time.After(50 * time.Millisecond):
		// Correct: still waiting.
	}

	errs <- context.Canceled
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("err = %v, want nil for a clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return once both components reported")
	}

	// Cancellation must be triggered once the first component finishes, so a component
	// failing on its own brings the other down rather than orphaning it.
	if stop.calls == 0 {
		t.Error("shutdown was never triggered")
	}
}

func TestWaitForShutdownSurfacesRealErrors(t *testing.T) {
	tests := []struct {
		name    string
		first   error
		second  error
		wantErr string
	}{
		{
			name:  "both clean",
			first: http.ErrServerClosed, second: context.Canceled,
		},
		{
			name:  "nil counts as clean",
			first: nil, second: nil,
		},
		{
			name:  "first component failed",
			first: errors.New("listen: address already in use"), second: context.Canceled,
			wantErr: "address already in use",
		},
		{
			name:  "second component failed",
			first: http.ErrServerClosed, second: errors.New("reconcile: broker unreachable"),
			wantErr: "broker unreachable",
		},
		{
			name:  "a real error wins over a clean one regardless of order",
			first: errors.New("store closed unexpectedly"), second: nil,
			wantErr: "store closed unexpectedly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := make(chan error, 2)
			errs <- tt.first
			errs <- tt.second

			err := waitForShutdown(errs, 2, &recordingStop{}, time.Second, testLogger())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A component that never stops must not hang the process forever — Docker's SIGKILL
// should be the backstop, not the normal path.
func TestWaitForShutdownHonoursGrace(t *testing.T) {
	errs := make(chan error, 2)
	errs <- context.Canceled // only one component ever reports

	start := time.Now()
	err := waitForShutdown(errs, 2, &recordingStop{}, 80*time.Millisecond, testLogger())
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("err = %v, want nil: the straggler is reported in the log, not as a failure", err)
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("returned after %v, want it to wait out the grace period", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v: the grace period is not bounding the wait", elapsed)
	}
}

func TestIsCleanShutdown(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, http.ErrServerClosed} {
		if !isCleanShutdown(err) {
			t.Errorf("isCleanShutdown(%v) = false, want true", err)
		}
	}
	if isCleanShutdown(errors.New("boom")) {
		t.Error("a real error must not count as a clean shutdown")
	}
}

// A server listening on ":8080" binds every interface, but that is not a dialable
// address — the healthcheck has to turn it into one.
func TestDialableAddr(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: ":8080", want: "127.0.0.1:8080"},
		{in: "0.0.0.0:8080", want: "127.0.0.1:8080"},
		{in: "[::]:8080", want: "127.0.0.1:8080"},
		{in: "127.0.0.1:9000", want: "127.0.0.1:9000"},
		{in: "localhost:8080", want: "localhost:8080"},
		{in: "8080", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := dialableAddr(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got %q", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("dialableAddr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
