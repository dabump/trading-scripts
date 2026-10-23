// Package claudelogin bridges the Claude Code CLI's login flow through the web
// page. OAuth cannot complete on its own inside a container: the browser callback
// it normally uses to finish the handshake has nowhere to land, so the CLI falls
// back to printing a URL and (usually) a code, and expects a human to open that URL
// in a browser that actually has a display and paste whatever it shows back into
// the CLI's own terminal. Headless scripting cannot skip that human step, so this
// package instead runs the CLI under a pty — so it behaves as if it were in an
// interactive terminal — captures its output for the page to display, and relays
// whatever the page's user types back into it.
//
// A session holds at most one login attempt at a time: a container has exactly one
// Claude CLI and one `.claude` config directory, so a second attempt stomping the
// first one's pty would garble both.
package claudelogin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/creack/pty"
)

// State is where a login attempt currently stands.
type State string

const (
	StateIdle      State = "idle"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

// maxOutputBytes bounds the captured terminal output. The login flow prints at
// most a few hundred bytes of prompt text; this is only a backstop against a
// runaway or chatty CLI holding memory for the life of the process.
const maxOutputBytes = 64 * 1024

// defaultTimeout bounds how long an attempt may sit unanswered before it is
// killed, so an abandoned browser tab doesn't leave a process — and its pty —
// running for the life of the container.
const defaultTimeout = 10 * time.Minute

// Status is a snapshot of a Session for a caller that cannot hold the lock.
type Status struct {
	State  State
	Output string
	Error  string
}

// Session manages one Claude CLI login attempt.
type Session struct {
	// Command and Args name the CLI to run, and default to "claude" "auth"
	// "login". "login" alone is not a real subcommand — the CLI would parse it
	// as a chat prompt instead, which launches a full interactive session (and,
	// on a fresh ~/.claude, a first-run wizard with an arrow-key theme picker
	// ahead of it) rather than going straight to signing in.
	// Fields rather than constants so a test can point this at a fake script
	// instead of the real binary — which the Go build stage, and most
	// developer machines, do not have installed.
	Command string
	Args    []string
	// Timeout overrides defaultTimeout when non-zero.
	Timeout time.Duration

	mu     sync.Mutex
	state  State
	output bytes.Buffer
	errMsg string
	ptmx   *os.File
	cancel context.CancelFunc
}

// New returns an idle session that runs the real Claude CLI.
func New() *Session {
	return &Session{state: StateIdle}
}

// Available reports whether the configured CLI can be found on PATH. The button
// uses this to explain its own absence rather than fail obscurely when clicked.
func (s *Session) Available() bool {
	command := s.Command
	if command == "" {
		command = "claude"
	}
	_, err := exec.LookPath(command)
	return err == nil
}

// LoggedIn reports whether the CLI's own credential file is present. This is read
// straight off disk rather than cached, so it reflects credentials from a previous
// container run (persisted via a mounted volume) even though a fresh Session starts
// in StateIdle with no memory of that earlier login.
func (s *Session) LoggedIn() bool {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = filepath.Join(home, ".claude")
	}
	_, err := os.Stat(filepath.Join(dir, ".credentials.json"))
	return err == nil
}

// Status returns a snapshot of the current attempt.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{State: s.state, Output: s.output.String(), Error: s.errMsg}
}

// Start begins a new login attempt under a pty. It refuses while one is already
// running rather than queuing — a second attempt would be talking to the same
// pty as the first and garble both.
func (s *Session) Start() error {
	s.mu.Lock()
	if s.state == StateRunning {
		s.mu.Unlock()
		return errors.New("a login is already in progress")
	}
	command := s.Command
	if command == "" {
		command = "claude"
	}
	args := s.Args
	if args == nil {
		args = []string{"auth", "login"}
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	s.output.Reset()
	s.errMsg = ""
	s.state = StateRunning
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, command, args...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		cancel()
		s.mu.Lock()
		s.state = StateFailed
		s.errMsg = fmt.Sprintf("could not start %s: %v", command, err)
		s.mu.Unlock()
		return nil
	}

	s.mu.Lock()
	s.ptmx = ptmx
	s.cancel = cancel
	s.mu.Unlock()

	go s.run(cmd, ptmx, cancel)
	return nil
}

// run reads the pty until the child closes it, then records the outcome. It owns
// cmd and ptmx for their whole lifetime, so nothing else touches them once Start
// has handed off to it.
func (s *Session) run(cmd *exec.Cmd, ptmx *os.File, cancel context.CancelFunc) {
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			s.appendOutput(buf[:n])
		}
		if err != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	ptmx.Close()
	cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptmx = nil
	s.cancel = nil
	if waitErr != nil {
		s.state = StateFailed
		s.errMsg = waitErr.Error()
		return
	}
	s.state = StateSucceeded
}

// ansiEscape matches the terminal escape sequences the CLI prints even on its
// plain auth-login path — notably OSC 8 ("]8;;URL<ESC>\") around the login link,
// which a real terminal renders as a clickable link but a browser's <pre> would
// render as stray invisible control bytes around the URL text.
var ansiEscape = regexp.MustCompile(`\x1b(?:\][^\x07\x1b]*(?:\x07|\x1b\\)|\[[0-9;?]*[a-zA-Z])`)

func (s *Session) appendOutput(p []byte) {
	p = ansiEscape.ReplaceAll(p, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output.Write(p)
	if s.output.Len() > maxOutputBytes {
		tail := s.output.Bytes()[s.output.Len()-maxOutputBytes:]
		kept := append([]byte(nil), tail...)
		s.output.Reset()
		s.output.Write(kept)
	}
}

// SendLine relays a line of input — typically a pasted URL or an auth code — to
// the running CLI, as if it had been typed into its terminal.
func (s *Session) SendLine(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateRunning || s.ptmx == nil {
		return errors.New("no login is waiting for input")
	}
	_, err := s.ptmx.Write([]byte(text + "\n"))
	return err
}

// Cancel kills an attempt in progress. Its exit is then reported as a normal
// failure by run, same as a timeout.
func (s *Session) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateRunning || s.cancel == nil {
		return errors.New("no login is in progress")
	}
	s.cancel()
	return nil
}
