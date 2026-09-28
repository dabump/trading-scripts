package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/config"
)

// The status page counts down to the next open, which after a close or over a weekend
// is not today's session — so this must return the following one.
func TestNextSessionReturnsTheFollowingSession(t *testing.T) {
	var gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		// A Friday request: the calendar skips the weekend.
		w.Write([]byte(`[
			{"date":"2026-09-28","open":"09:30","close":"16:00"},
			{"date":"2026-09-29","open":"09:30","close":"16:00"}
		]`))
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	day, err := a.NextSession(context.Background(), "2026-09-25")
	if err != nil {
		t.Fatal(err)
	}

	// The window must start the day after, so today can never be returned as "next".
	if gotStart != "2026-09-26" {
		t.Errorf("start = %q, want the day after the given date", gotStart)
	}
	if gotEnd == "" {
		t.Error("an end date is required or the calendar range is unbounded")
	}
	if day.Date != "2026-09-28" {
		t.Errorf("date = %q, want the earliest following session", day.Date)
	}
	if got := day.Open.Format("15:04"); got != "09:30" {
		t.Errorf("open = %s, want 09:30 ET", got)
	}
}

// An early close on the next session must be reported as such, not assumed to be 16:00.
func TestNextSessionCarriesEarlyClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"date":"2026-11-27","open":"09:30","close":"13:00"}]`))
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	day, err := a.NextSession(context.Background(), "2026-11-26")
	if err != nil {
		t.Fatal(err)
	}
	if got := day.Close.Format("15:04"); got != "13:00" {
		t.Errorf("close = %s, want 13:00 (early close)", got)
	}
}

// No session in the window is normal, not an error: the page shows no countdown
// rather than a wrong one.
func TestNextSessionEmptyWindowIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	day, err := a.NextSession(context.Background(), "2026-09-25")
	if err != nil {
		t.Fatalf("an empty window must not error: %v", err)
	}
	if !day.Open.IsZero() {
		t.Errorf("got %+v, want a zero day", day)
	}
}

// A day at or before the given date must never be returned, even if the API includes it.
func TestNextSessionSkipsTheGivenDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"date":"2026-09-25","open":"09:30","close":"16:00"},
			{"date":"2026-09-28","open":"09:30","close":"16:00"}
		]`))
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	day, err := a.NextSession(context.Background(), "2026-09-25")
	if err != nil {
		t.Fatal(err)
	}
	if day.Date != "2026-09-28" {
		t.Errorf("date = %q, want the given date to be skipped", day.Date)
	}
}

func TestNextSessionRejectsBadDate(t *testing.T) {
	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: "http://x", DataURL: "http://x"}, "sip")
	if _, err := a.NextSession(context.Background(), "not-a-date"); err == nil {
		t.Error("want an error for a malformed date")
	}
}
