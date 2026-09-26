package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
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
}

type Server struct {
	cfg    *config.Config
	store  *store.Store
	engine StateSource
	tmpl   *template.Template
	log    *slog.Logger
	now    func() time.Time
	paper  bool
	// floatAvailable records whether a float data source is actually wired up;
	// without one nothing can qualify, which the page has to say out loud.
	floatAvailable bool
}

func NewServer(cfg *config.Config, st *store.Store, eng StateSource, logger *slog.Logger, now func() time.Time, paper, floatAvailable bool) (*Server, error) {
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
	return &Server{cfg: cfg, store: st, engine: eng, tmpl: tmpl, log: logger,
		now: now, paper: paper, floatAvailable: floatAvailable}, nil
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/fragment", s.handleFragment)
	mux.HandleFunc("/api/status", s.handleJSON)
	return mux
}

func (s *Server) view() (*View, error) {
	state, errMsg := s.engine.State()
	sess, tradingDay := s.engine.Session()
	return BuildView(s.cfg, s.store, state, errMsg, sess, tradingDay, s.now(), s.paper, s.floatAvailable)
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
