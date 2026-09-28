package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/martincoetzee/trading-agent/internal/domain"
)

// The container healthcheck points here, so what it reports has to mean something. The
// distinction that matters: a bearish halt is the kill switch working, not a fault.
func TestHealthzReflectsAgentHealth(t *testing.T) {
	tests := []struct {
		name       string
		state      domain.AgentState
		errMsg     string
		wantStatus int
	}{
		{"screening is healthy", domain.StateScreening, "", http.StatusOK},
		{"market closed is healthy", domain.StateMarketClosed, "", http.StatusOK},
		{"sentiment check is healthy", domain.StateSentimentCheck, "", http.StatusOK},
		{"eod window is healthy", domain.StateEODWindow, "", http.StatusOK},
		{
			// Deliberate: the agent decided not to trade, which is it working correctly.
			name:  "a bearish halt is healthy",
			state: domain.StateHaltedBearish, wantStatus: http.StatusOK,
		},
		{
			name:  "a fault is unhealthy",
			state: domain.StateError, errMsg: "poll sentiment: upstream 503",
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.eng.state = tt.state
			f.eng.errMsg = tt.errMsg

			code, body := f.get(t, "/healthz")
			if code != tt.wantStatus {
				t.Errorf("status = %d, want %d", code, tt.wantStatus)
			}
			if !strings.Contains(body, string(tt.state)) {
				t.Errorf("body = %q, should name the state", body)
			}
			// The message goes in the body so `docker inspect` explains itself.
			if tt.errMsg != "" && !strings.Contains(body, tt.errMsg) {
				t.Errorf("body = %q, should carry the fault message", body)
			}
		})
	}
}

// /api/status must stay 200 in every state: it is an information endpoint, and the split
// from /healthz is what lets the healthcheck be meaningful.
func TestStatusEndpointStays200WhenFaulted(t *testing.T) {
	f := newFixture(t)
	f.eng.state = domain.StateError
	f.eng.errMsg = "upstream 503"

	if code, _ := f.get(t, "/api/status"); code != http.StatusOK {
		t.Errorf("/api/status = %d, want 200 even when faulted", code)
	}
	if code, _ := f.get(t, "/healthz"); code != http.StatusServiceUnavailable {
		t.Errorf("/healthz = %d, want 503 when faulted", code)
	}
}
