package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

// StateSource is the engine, from the page's point of view: something that can be
// asked what it is currently doing. Keeping it an interface means the web package
// does not depend on the engine.
type StateSource interface {
	State() (domain.AgentState, string)
	Session() (scheduler.Session, bool)
	// NextSession is the first session after today, for the countdown to the next
	// open. The web layer cannot ask the broker itself, so the engine caches it.
	NextSession() (scheduler.Session, bool)
	// Account is the last balance the trading loop read, for the same reason: the
	// page shows the broker's cash without a page poll costing an API call.
	Account() domain.AccountSnapshot
}

// Actions are the manually triggered operations behind the page's buttons.
//
// The two checks are read-only by contract: they evaluate and report, never persist
// and never place an order. Keeping that promise is the implementation's job, not the
// handler's — see engine.CheckSentiment and engine.ScreenNow.
//
// ClosePosition and OpenPosition trade. They exist because the operator needs both
// directions: getting out of a position that is going wrong, and taking one the
// mechanical setup gate refused. In both cases the engine decides whether the action
// is possible and on what terms — the handlers carry only an id or a symbol.
type Actions interface {
	CheckSentiment(ctx context.Context) (domain.SentimentCheck, error)
	ScreenNow(ctx context.Context) (domain.ScreenPreview, error)
	ClosePosition(ctx context.Context, id int64) (domain.Position, error)
	OpenPosition(ctx context.Context, symbol string) (domain.ManualOpen, error)
	// IgnoreHalt dismisses a halt the gate could not judge. It does not trade; it
	// only lets the automated path resume.
	IgnoreHalt(ctx context.Context) error
}

type Server struct {
	cfg     *config.Config
	store   *store.Store
	engine  StateSource
	tmpl    *template.Template
	log     *slog.Logger
	actions Actions
	now     func() time.Time
	paper   bool
}

func NewServer(cfg *config.Config, st *store.Store, eng StateSource, actions Actions, logger *slog.Logger, now func() time.Time, paper bool) (*Server, error) {
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, store: st, engine: eng, actions: actions, tmpl: tmpl,
		log: logger, now: now, paper: paper}, nil
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/fragment", s.handleFragment)
	mux.HandleFunc("/api/status", s.handleJSON)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/actions/sentiment", s.handleCheckSentiment)
	mux.HandleFunc("/actions/screen", s.handleScreenNow)
	mux.HandleFunc("/actions/close", s.handleClosePosition)
	mux.HandleFunc("/actions/open", s.handleOpenPosition)
	mux.HandleFunc("/actions/ignore-halt", s.handleIgnoreHalt)
	return mux
}

// actionTimeout bounds a manual check. Screening fans out to several per-symbol
// calls, and a hung upstream should return an error to the modal rather than
// leaving the button spinning forever.
const actionTimeout = 45 * time.Second

// SentimentModal is what the sentiment popup renders.
type SentimentModal struct {
	Title          string
	TakenAt        string
	Rows           []SentimentModalRow
	Classification string
	Tone           string
	Verdict        string
	Missing        []string
	Threshold      string
	Persisted      bool
}

type SentimentModalRow struct {
	Symbol string
	Change string
	Tone   string
}

// ScreenModal is what the screening popup renders.
type ScreenModal struct {
	Title        string
	TakenAt      string
	UniverseSize int
	MarketOpen   bool
	Columns      []string
	Rows         []ScreenRow
	// PassCount is how many rows qualified; Evaluated is how many symbols cleared
	// the price-move filter and were therefore looked at in full. The gap between
	// Evaluated and UniverseSize is the whole market that was scanned cheaply.
	PassCount int
	Evaluated int
}

// handleCheckSentiment runs the sentiment check and renders the modal body.
func (s *Server) handleCheckSentiment(w http.ResponseWriter, r *http.Request) {
	// POST only: these trigger upstream work, so a prefetch or a crawler following
	// a link must not be able to set them off.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.actions == nil {
		s.renderActionError(w, "Manual checks are not available.", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	check, err := s.actions.CheckSentiment(ctx)
	if err != nil {
		s.log.Warn("manual sentiment check failed", "err", err)
		s.renderActionError(w, err.Error(), http.StatusOK)
		return
	}

	m := &SentimentModal{
		Title:          "Sentiment check",
		TakenAt:        check.TakenAt.In(scheduler.ET).Format("15:04:05 MST"),
		Classification: string(check.Classification),
		Missing:        check.Missing,
		Threshold: fmt.Sprintf("bearish at avg ≤ %.1f%%%s",
			s.cfg.Sentiment.BearishAvgPct,
			map[bool]string{true: " with none positive", false: ""}[s.cfg.Sentiment.RequireAllNegative]),
	}
	for _, sym := range s.cfg.Sentiment.Symbols {
		v, ok := check.Percentages[sym]
		row := SentimentModalRow{Symbol: sym, Change: "—", Tone: "idle"}
		if ok {
			row.Change = pct(v)
			row.Tone = toneForPnL(v)
		}
		m.Rows = append(m.Rows, row)
	}
	switch check.Classification {
	case domain.VerdictBearish:
		m.Tone, m.Verdict = "bad", "Overwhelmingly bearish — this would halt trading for the session."
	case domain.VerdictProceed:
		m.Tone, m.Verdict = "good", "Not overwhelmingly bearish — this would allow screening to proceed."
	default:
		m.Tone, m.Verdict = "idle", "Not enough data to classify."
	}

	s.renderModal(w, "sentimentModal", m)
}

// handleScreenNow runs a screening pass and renders the modal body.
func (s *Server) handleScreenNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.actions == nil {
		s.renderActionError(w, "Manual checks are not available.", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	preview, err := s.actions.ScreenNow(ctx)
	if err != nil {
		s.log.Warn("manual screening failed", "err", err)
		s.renderActionError(w, err.Error(), http.StatusOK)
		return
	}

	m := &ScreenModal{
		Title:        "Candidate screen",
		TakenAt:      preview.TakenAt.In(scheduler.ET).Format("15:04:05 MST"),
		UniverseSize: preview.UniverseSize,
		MarketOpen:   preview.MarketOpen,
	}
	for i, e := range preview.Evaluations {
		if i == 0 {
			for _, c := range e.Criteria {
				m.Columns = append(m.Columns, c.Name)
			}
		}
		row := ScreenRow{Symbol: e.Symbol, Qualifies: e.Qualifies, FailReason: e.FailReason}
		for _, c := range e.Criteria {
			row.Cells = append(row.Cells, CriterionCell{Pass: c.Pass, Display: c.Display})
		}
		if e.Qualifies {
			// The manual screen now applies exactly the criteria the automated scan
			// does, so "qualifies" is the truth: during trading hours the agent would
			// act on this row.
			row.Verdict = "Qualifies"
			m.PassCount++
		} else {
			row.Verdict = e.FailReason
		}
		m.Rows = append(m.Rows, row)
	}
	m.Evaluated = len(m.Rows)

	s.renderModal(w, "screenModal", m)
}

// CloseModal is what the confirmation popup renders after a manual close.
type CloseModal struct {
	Title      string
	Symbol     string
	Shares     int
	ExitPrice  string
	EntryPrice string
	PnLDollars string
	PnLPct     string
	Tone       string
	// TakenAt is the name the shared modalHead partial reads.
	TakenAt string
}

// handleClosePosition sells a position on the operator's instruction.
//
// POST only, like the other actions: this one places an order, so a prefetch or a
// crawler following a link must not be able to set it off.
func (s *Server) handleClosePosition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.actions == nil {
		s.renderActionError(w, "Manual actions are not available.", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		s.renderActionError(w, "That position id is not valid.", http.StatusOK)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	closed, err := s.actions.ClosePosition(ctx, id)
	if err != nil {
		s.log.Warn("manual close failed", "position", id, "err", err)
		s.renderActionError(w, err.Error(), http.StatusOK)
		return
	}

	pnl := closed.RealizedDollars()
	s.renderModal(w, "closeModal", &CloseModal{
		Title:      "Position closed",
		Symbol:     closed.Symbol,
		Shares:     closed.Shares,
		EntryPrice: money(closed.EntryPrice),
		ExitPrice:  money(closed.ExitPrice),
		PnLDollars: signedMoney(pnl),
		PnLPct:     pct(closed.RealizedPct()),
		Tone:       toneForPnL(pnl),
		TakenAt:    closed.ExitTime.In(scheduler.ET).Format("15:04:05 MST"),
	})
}

// OpenModal is the confirmation after a manual open. It leads with the size and the
// stop, because those are what the operator has just committed to and the last moment
// to notice they are not what was expected.
type OpenModal struct {
	Title   string
	TakenAt string
	Symbol  string
	Shares  int
	Entry   string
	Stop    string
	// StopSource says whether the market drew the stop or the config did.
	StopSource  string
	SetupReason string
	Risk        string
	Notional    string
	Halted      bool
}

// handleOpenPosition buys a screened candidate on the operator's instruction.
func (s *Server) handleOpenPosition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.actions == nil {
		s.renderActionError(w, "Manual actions are not available.", http.StatusServiceUnavailable)
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
	if symbol == "" {
		s.renderActionError(w, "No symbol was given.", http.StatusOK)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	res, err := s.actions.OpenPosition(ctx, symbol)
	if err != nil {
		s.log.Warn("manual open failed", "symbol", symbol, "err", err)
		s.renderActionError(w, err.Error(), http.StatusOK)
		return
	}

	m := &OpenModal{
		Title:    "Position opened",
		TakenAt:  res.Position.EntryTime.In(scheduler.ET).Format("15:04:05 MST"),
		Symbol:   res.Position.Symbol,
		Shares:   res.Shares,
		Entry:    money(res.Entry),
		Stop:     money(res.Stop),
		Risk:     money(res.RiskDollar),
		Notional: accountMoney(float64(res.Shares) * res.Entry),
		Halted:   res.Halted,
	}
	if res.FromSetup {
		m.StopSource = "from the chart — a completed pullback, the same stop the agent would have used"
	} else {
		m.StopSource = fmt.Sprintf("the configured maximum of %s, because there was no setup to read one from",
			trimNumber(s.cfg.Entry.MaxStopDistancePct)+"%")
		m.SetupReason = res.SetupReason
	}
	s.renderModal(w, "openModal", m)
}

// handleIgnoreHalt dismisses a halt that exists only because no readings were taken.
func (s *Server) handleIgnoreHalt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.actions == nil {
		s.renderActionError(w, "Manual actions are not available.", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()

	if err := s.actions.IgnoreHalt(ctx); err != nil {
		s.log.Warn("halt override refused", "err", err)
		s.renderActionError(w, err.Error(), http.StatusOK)
		return
	}
	s.renderModal(w, "ignoreHaltModal", struct{ Title, TakenAt string }{
		Title:   "Halt dismissed",
		TakenAt: s.now().In(scheduler.ET).Format("15:04:05 MST"),
	})
}

func (s *Server) renderModal(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render modal", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// renderActionError reports a failed check inside the modal, so an upstream
// outage shows up where the user is looking rather than only in the log.
func (s *Server) renderActionError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "actionError", msg); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

func (s *Server) view() (*View, error) {
	state, errMsg := s.engine.State()
	sess, tradingDay := s.engine.Session()
	next, nextKnown := s.engine.NextSession()
	return BuildView(s.cfg, s.store, state, errMsg, sess, tradingDay, next, nextKnown,
		s.engine.Account(), s.now(), s.paper)
}

// render writes to a buffer first so a template error produces a clean 500
// instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, name string, v *View) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, v); err != nil {
		s.log.Error("render template", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	v, err := s.view()
	if err != nil {
		s.log.Error("build view", "err", err)
		http.Error(w, "failed to read state", http.StatusInternalServerError)
		return
	}
	s.render(w, "page", v)
}

func (s *Server) handleFragment(w http.ResponseWriter, r *http.Request) {
	v, err := s.view()
	if err != nil {
		s.log.Error("build view", "err", err)
		http.Error(w, "failed to read state", http.StatusInternalServerError)
		return
	}
	s.render(w, "content", v)
}

// handleHealth reports whether the agent is faulted, for the container healthcheck.
//
// It is deliberately separate from /api/status, which answers 200 in every state
// because it is an information endpoint. Pointing a healthcheck at that would report
// healthy while the agent sat in ERROR unable to trade.
//
// A bearish halt is healthy: it is the kill switch working as designed, not a fault.
// Only StateError is unhealthy.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	state, errMsg := s.engine.State()

	status := http.StatusOK
	if state == domain.StateError {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The message goes in the body so `docker inspect` output explains itself.
	if errMsg != "" {
		fmt.Fprintf(w, "%s: %s\n", state, errMsg)
		return
	}
	fmt.Fprintf(w, "%s\n", state)
}

// handleJSON exposes the same state as machine-readable output, which is what
// makes the running daemon verifiable without scraping HTML.
func (s *Server) handleJSON(w http.ResponseWriter, r *http.Request) {
	v, err := s.view()
	if err != nil {
		http.Error(w, "failed to read state", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.log.Error("encode status", "err", err)
	}
}

// Serve runs the HTTP server until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Web.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	s.log.Info("status page listening", "addr", s.cfg.Web.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
