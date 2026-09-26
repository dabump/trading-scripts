package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
)

// A busy morning produces more stories than one page holds. Without pagination the
// symbols whose news falls beyond the first page report zero, which reads as "no
// catalyst" and silently disqualifies them.
func TestNewsCountsPaginatesBeyondTheFirstPage(t *testing.T) {
	// 60 symbols, each with exactly one story: more than the 50-per-page maximum.
	symbols := make([]string, 60)
	for i := range symbols {
		symbols[i] = fmt.Sprintf("SYM%02d", i)
	}

	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		q := r.URL.Query()
		if got := q.Get("limit"); got != "50" {
			t.Errorf("limit = %q, want the API maximum of 50", got)
		}
		requested := strings.Split(q.Get("symbols"), ",")

		// Serve this batch's symbols 50 at a time, with a continuation token while
		// more remain.
		offset := 0
		if tok := q.Get("page_token"); tok != "" {
			fmt.Sscanf(tok, "%d", &offset)
		}
		end := offset + 50
		if end > len(requested) {
			end = len(requested)
		}

		type item struct {
			Symbols []string `json:"symbols"`
		}
		out := struct {
			News          []item  `json:"news"`
			NextPageToken *string `json:"next_page_token"`
		}{}
		for _, sym := range requested[offset:end] {
			out.News = append(out.News, item{Symbols: []string{sym}})
		}
		if end < len(requested) {
			tok := fmt.Sprintf("%d", end)
			out.NextPageToken = &tok
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{
		APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL,
	}, "sip")

	got, err := a.NewsCounts(context.Background(), symbols, time.Now().Add(-4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// Every symbol had a story, so every symbol must be found.
	var missing []string
	for _, sym := range symbols {
		if got[sym] == 0 {
			missing = append(missing, sym)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d symbols reported no news despite each having a story: %v",
			len(missing), len(symbols), missing[:min(5, len(missing))])
	}
	if pages < 2 {
		t.Errorf("made %d request(s); more than 50 stories requires pagination", pages)
	}
}

// Stories are counted per symbol, and a symbol with several stories still counts as
// one catalyst as far as the criterion goes — but the count is displayed, so it
// should be accurate.
func TestNewsCountsAccumulatesAcrossPages(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			io.WriteString(w, `{"news":[{"symbols":["ABCD"]},{"symbols":["ABCD"]}],"next_page_token":"p2"}`)
			return
		}
		io.WriteString(w, `{"news":[{"symbols":["ABCD"]}],"next_page_token":null}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	got, err := a.NewsCounts(context.Background(), []string{"ABCD"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got["ABCD"] != 3 {
		t.Errorf("ABCD = %d stories, want 3 accumulated across both pages", got["ABCD"])
	}
}

// A malformed or endlessly-repeating continuation token must not spin forever.
func TestNewsCountsStopsOnRepeatedPageToken(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"news":[{"symbols":["ABCD"]}],"next_page_token":"same"}`)
	}))
	defer srv.Close()

	a := NewAlpaca(&config.Secrets{APIKey: "k", APISecret: "s", BaseURL: srv.URL, DataURL: srv.URL}, "sip")
	done := make(chan struct{})
	go func() {
		a.NewsCounts(context.Background(), []string{"ABCD"}, time.Now().Add(-time.Hour))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("NewsCounts did not terminate on a repeating page token")
	}
	if calls > 40 {
		t.Errorf("made %d requests; pagination needs a page cap", calls)
	}
}
