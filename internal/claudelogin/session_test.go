package claudelogin

import (
	"strings"
	"testing"
	"time"
)

// waitForState polls until the session reaches one of the wanted states, or fails
// the test. The CLI (real or faked) runs as a subprocess on its own schedule, so
// polling is the only option — there is no channel to block on from outside.
func waitForState(t *testing.T, s *Session, timeout time.Duration, want ...State) Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := s.Status()
		for _, w := range want {
			if st.State == w {
				return st
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s after %s, want one of %v (output: %q)", st.State, timeout, want, st.Output)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A script standing in for `claude login`: it prints a prompt, waits for a line
// of input, echoes it back, then exits cleanly. This is what the manual
// URL-then-code exchange looks like from the outside.
const fakeLoginScript = `echo 'open https://example.com/device and enter the code shown there'
read -r code
echo "got code: $code"
exit 0
`

func newFakeSession(script string) *Session {
	return &Session{Command: "sh", Args: []string{"-c", script}}
}

func TestLoginRoundTrip(t *testing.T) {
	s := newFakeSession(fakeLoginScript)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	waitForOutput(t, s, 2*time.Second, "open https://example.com/device")

	if err := s.SendLine("ABC123"); err != nil {
		t.Fatalf("SendLine: %v", err)
	}

	st := waitForState(t, s, 2*time.Second, StateSucceeded, StateFailed)
	if st.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded (output: %q, err: %q)", st.State, st.Output, st.Error)
	}
	if !strings.Contains(st.Output, "got code: ABC123") {
		t.Errorf("output = %q, want it to contain the echoed code", st.Output)
	}
}

func waitForOutput(t *testing.T, s *Session, timeout time.Duration, want string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := s.Status()
		if strings.Contains(st.Output, want) {
			return
		}
		if st.State == StateFailed || st.State == StateSucceeded {
			t.Fatalf("session ended (%s) before printing %q; output: %q", st.State, want, st.Output)
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q after %s; got %q", want, timeout, st.Output)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLoginFailureIsReported(t *testing.T) {
	s := newFakeSession("echo 'nope'; exit 1\n")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	st := waitForState(t, s, 2*time.Second, StateFailed, StateSucceeded)
	if st.State != StateFailed {
		t.Fatalf("state = %s, want failed", st.State)
	}
	if st.Error == "" {
		t.Error("a failed attempt should record why")
	}
}

func TestConcurrentStartIsRefused(t *testing.T) {
	s := newFakeSession("sleep 1\nexit 0\n")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Cancel()

	if err := s.Start(); err == nil {
		t.Error("a second Start while one is running should be refused")
	}
}

func TestSendLineWithoutARunningSessionIsRefused(t *testing.T) {
	s := New()
	if err := s.SendLine("x"); err == nil {
		t.Error("SendLine on an idle session should be refused")
	}
}

func TestCancelStopsARunningAttempt(t *testing.T) {
	s := newFakeSession("sleep 30\nexit 0\n")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	if err := s.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	st := waitForState(t, s, 2*time.Second, StateFailed, StateSucceeded)
	if st.State != StateFailed {
		t.Fatalf("state = %s, want failed after cancel", st.State)
	}
}

func TestTimeoutKillsAnAbandonedAttempt(t *testing.T) {
	s := newFakeSession("sleep 30\nexit 0\n")
	s.Timeout = 100 * time.Millisecond
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	waitForState(t, s, 2*time.Second, StateFailed, StateSucceeded)
}

func TestAvailableReportsWhetherTheCommandExists(t *testing.T) {
	if (&Session{Command: "sh"}).Available() != true {
		t.Error("sh should be found on PATH in any environment this test runs in")
	}
	if (&Session{Command: "definitely-not-a-real-binary-xyz"}).Available() {
		t.Error("a nonexistent command should report unavailable")
	}
}

// The real CLI wraps its login URL in an OSC 8 hyperlink escape — ESC]8;;<url>ESC\
// <visible text> ESC]8;;ESC\ — which a real terminal renders as the visible text
// made clickable, hiding the <url> part. A browser's <pre> does not interpret OSC
// 8 at all, so without stripping it the hidden url and the raw escape bytes would
// both show up as stray text around the visible part.
func TestOutputStripsTerminalHyperlinkEscapes(t *testing.T) {
	s := newFakeSession("printf 'visit: \\033]8;;https://example.com/login\\033\\\\here\\033]8;;\\033\\\\\\n'; sleep 30\n")
	defer s.Cancel()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, s, 2*time.Second, "visit: here")
	out := s.Status().Output
	if strings.Contains(out, "example.com") || strings.ContainsAny(out, "\x1b") {
		t.Errorf("output should show only the link's visible text, got %q", out)
	}
}

func TestLoggedInIsFalseWithNoCredentialFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := New()
	if s.LoggedIn() {
		t.Error("an empty config dir should not report logged in")
	}
}
