// Command agent runs the trading daemon and its status page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/martincoetzee/trading-agent/internal/audit"
	"github.com/martincoetzee/trading-agent/internal/broker"
	"github.com/martincoetzee/trading-agent/internal/config"
	"github.com/martincoetzee/trading-agent/internal/domain"
	"github.com/martincoetzee/trading-agent/internal/engine"
	"github.com/martincoetzee/trading-agent/internal/scheduler"
	"github.com/martincoetzee/trading-agent/internal/store"
	"github.com/martincoetzee/trading-agent/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "config/config.yaml", "path to the config file")
		offline    = flag.Bool("offline", false,
			"run against an in-memory fake broker with seeded data; no credentials, no network, no real orders")
		allowLive = flag.Bool("allow-live-trading", false,
			"required to start when ALPACA_BASE_URL points at the live endpoint")
		verbose     = flag.Bool("verbose", false, "enable debug logging")
		healthcheck = flag.Bool("healthcheck", false,
			"probe a running agent's /healthz and exit 0 if healthy; used by the container healthcheck")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Before any store, audit or broker setup: the probe must be cheap and must not
	// touch the running agent's state.
	if *healthcheck {
		return probeHealth(cfg.Web.ListenAddr)
	}

	if dir := filepath.Dir(cfg.Storage.DatabasePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create database directory: %w", err)
		}
	}
	st, err := store.Open(cfg.Storage.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()

	auditLog, err := audit.Open(cfg.Audit.Directory)
	if err != nil {
		return err
	}
	defer auditLog.Close()

	var (
		data    broker.MarketData
		trading broker.Trading
		paper   = true
	)

	if *offline {
		logger.Warn("offline mode: using a fake broker with seeded data; no real orders will be placed")
		// The simulated session is compressed and anchored to now, so the whole
		// daily flow is observable in a couple of minutes at any time of day. A real
		// session's hour-long sentiment window would otherwise make offline mode
		// show nothing but SENTIMENT_CHECK.
		cfg.Timing.SentimentWindow = 30 * time.Second
		cfg.Timing.SentimentPollInterval = 10 * time.Second
		cfg.Timing.ScreenerScanInterval = 5 * time.Second
		cfg.Timing.PositionPollInterval = 2 * time.Second
		// The entry window has to stay open for most of the compressed session, or
		// the demo would screen and never buy.
		cfg.Timing.EntryWindow = 10 * time.Minute
		cfg.Exit.EODExitOffsetMins = 15
		logger.Warn("offline mode: session timings compressed",
			"sentiment_window", cfg.Timing.SentimentWindow,
			"scan_interval", cfg.Timing.ScreenerScanInterval)

		// Pre-market cannot be compressed the way the rest is: its start is a wall
		// clock time in exchange hours, not an offset from the open. So it is moved to
		// just before now instead, and seedFake pushes the simulated open a little into
		// the future — which is what makes the demo start in PRE_MARKET and walk
		// through the new state rather than skipping straight past it. "Just before"
		// is the length of DEMO's seeded chart: the setup is read on closed candles
		// only, so the chart has to have finished by the time the demo starts. Left
		// alone when the config has pre-market off, so the switch still means
		// something offline.
		if cfg.PreMarket.Enabled {
			cfg.PreMarket.Start = time.Now().Add(-offlineChartLen * cfg.Entry.PatternInterval).
				In(scheduler.ET).Format("15:04")
			cfg.PreMarket.ScanInterval = 5 * time.Second
			logger.Warn("offline mode: pre-market opened at the current clock time",
				"start", cfg.PreMarket.Start, "scan_interval", cfg.PreMarket.ScanInterval)
		}

		fake := seedFake(time.Now(), cfg)
		data, trading = fake, fake
	} else {
		secrets, err := config.LoadSecrets()
		if err != nil {
			return err
		}
		// Real money needs a deliberate opt-in, per docs/operations.md. The PDT
		// constraint documented there is also unresolved, so this guard is doing more
		// than protecting against a typo.
		if secrets.IsLive() && !*allowLive {
			return errors.New(
				"ALPACA_BASE_URL points at live trading. Re-run with -allow-live-trading if that is " +
					"really intended, and resolve the Pattern Day Trader constraint in " +
					"docs/operations.md first")
		}
		paper = !secrets.IsLive()

		alpaca := broker.NewAlpaca(secrets, cfg.MarketData.Feed)
		if cfg.MarketData.Feed == "iex" {
			logger.Warn("using the IEX feed, which carries only a few percent of " +
				"consolidated volume; the relative-volume screening criterion will not " +
				"reflect market-wide activity")
		}
		data, trading = alpaca, alpaca
	}

	eng := engine.New(engine.Deps{
		Config: cfg, Store: st, Data: data, Trading: trading,
		Logger: logger, Audit: auditLog,
	})

	// The effective configuration is recorded at startup so a later reader can tell
	// which thresholds were in force when a trade happened. Credentials are
	// deliberately absent: an audit trail must be safe to hand to someone.
	now := time.Now()
	if err := auditLog.Record(audit.Event{
		At:          now,
		SessionDate: scheduler.SessionDate(now),
		Kind:        audit.AgentStarted,
		Summary:     fmt.Sprintf("agent started (paper=%v, offline=%v)", paper, *offline),
		Detail: map[string]any{
			"paper":                    paper,
			"offline":                  *offline,
			"data_feed":                cfg.MarketData.Feed,
			"min_intraday_pct":         cfg.Screening.MinIntradayPct,
			"min_volume_multiple":      cfg.Screening.MinVolumeMultiple,
			"avg_volume_lookback_days": cfg.Screening.AvgVolumeLookbackDays,
			"news_lookback":            cfg.Screening.NewsLookback.String(),
			"max_enriched":             cfg.Screening.MaxEnriched,
			"max_price":                cfg.Screening.MaxPrice,
			"risk_per_trade_pct":       cfg.Risk.RiskPerTradePct,
			"max_position_pct":         cfg.Risk.MaxPositionPct,
			"entry_pattern_interval":   cfg.Entry.PatternInterval.String(),
			"entry_ema_period":         cfg.Entry.EMAPeriod,
			"entry_max_stop_pct":       cfg.Entry.MaxStopDistancePct,
			"first_target_r":           cfg.Exit.FirstTargetR,
			"first_target_fraction":    cfg.Exit.FirstTargetFraction,
			"entry_window":             cfg.Timing.EntryWindow.String(),
			"max_concurrent_positions": cfg.Risk.MaxConcurrentPositions,
			"stop_loss_pct":            cfg.Risk.StopLossPct,
			"min_price":                cfg.Screening.MinPrice,
			"min_dollar_volume":        cfg.Screening.MinDollarVolume,
			"eod_exit_offset_minutes":  cfg.Exit.EODExitOffsetMins,
			"order_type":               cfg.Execution.OrderType,
			"allow_same_day_reentry":   cfg.Risk.AllowSameDayReentry,
		},
	}); err != nil {
		return fmt.Errorf("record startup audit event: %w", err)
	}

	srv, err := web.NewServer(cfg, st, eng, eng, logger, time.Now, paper)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	const components = 2
	errs := make(chan error, components)
	go func() { errs <- srv.Serve(ctx) }()
	go func() { errs <- eng.Run(ctx) }()

	logger.Info("agent started", "paper", paper, "offline", *offline,
		"listen", cfg.Web.ListenAddr, "db", cfg.Storage.DatabasePath,
		"audit", auditLog.Path(scheduler.SessionDate(now)))

	if err := waitForShutdown(errs, components, &stopOnce{stop: stop}, shutdownGrace, logger); err != nil {
		return err
	}
	logger.Info("shutting down")
	return nil
}

// probeHealth asks a running agent whether it is healthy.
//
// It reads the port from the same config the server binds, which is the point: the
// container config is bind-mounted, so a hardcoded port in the healthcheck would leave
// the container permanently unhealthy after a listen_addr change while the daemon was
// perfectly fine.
func probeHealth(listenAddr string) error {
	addr, err := dialableAddr(listenAddr)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("healthcheck: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// dialableAddr turns a listen address into one that can be connected to. A server
// listening on ":8080" binds every interface, but ":8080" is not a valid dial target,
// so an empty host becomes loopback.
func dialableAddr(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse web.listen_addr %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// shutdownGrace bounds how long a component gets to stop after the others have. It sits
// below the compose stop_grace_period so Docker's SIGKILL is the backstop, not the
// normal path.
const shutdownGrace = 15 * time.Second

// stopOnce wraps the context cancel so waitForShutdown can trigger shutdown itself
// without caring whether a signal already did.
type stopOnce struct {
	once sync.Once
	stop func()
}

func (s *stopOnce) Stop() {
	s.once.Do(s.stop)
}

// waitForShutdown blocks until every component has reported, and returns the first real
// error among them.
//
// Waiting for all of them is the point. Both components exit on context cancellation,
// but the web server returns almost instantly while the engine may be mid-tick
// submitting an order or appending an audit event. Returning after the first would run
// the caller's deferred store and audit Close while the engine is still writing — losing
// an audit record, or writing against a closed database.
//
// The first component to finish also triggers cancellation, so one of them failing on its
// own brings the other down instead of leaving it orphaned.
func waitForShutdown(errs <-chan error, components int, stop interface{ Stop() }, grace time.Duration, log *slog.Logger) error {
	var firstErr error

	for i := 0; i < components; i++ {
		if i == 0 {
			err := <-errs
			stop.Stop()
			if isCleanShutdown(err) {
				err = nil
			}
			firstErr = err
			continue
		}

		select {
		case err := <-errs:
			if !isCleanShutdown(err) && firstErr == nil {
				firstErr = err
			}
		case <-time.After(grace):
			// Give up waiting rather than hang forever, but say so: anything the
			// straggler was midway through is now unaccounted for.
			log.Error("component did not stop within the grace period; "+
				"state may be incomplete", "grace", grace, "still_running", components-i)
			return firstErr
		}
	}
	return firstErr
}

// isCleanShutdown reports whether an error simply means "asked to stop".
func isCleanShutdown(err error) bool {
	return err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, http.ErrServerClosed)
}

// offlineChartLen is how many candles broker.Fake.SetSetupBars seeds.
const offlineChartLen = 16

// seedFake builds an offline broker with a plausible session and one candidate
// that clears every screening criterion, so the daemon and its page can be
// exercised end to end without credentials.
func seedFake(now time.Time, cfg *config.Config) *broker.Fake {
	// Anchored to now rather than 09:30-16:00 so offline mode is demonstrable
	// whatever the wall clock says. With the compressed timings set by the caller
	// this yields a ~30s sentiment window, ~4.5 minutes of trading, then the EOD
	// window.
	//
	// The open is pushed forward when pre-market is on, so the demo opens in
	// PRE_MARKET and the new state is actually observable; the caller has already
	// moved the pre-market start to now for the same reason.
	open := now
	if cfg.PreMarket.Enabled {
		open = now.Add(90 * time.Second)
	}
	close := open.Add(20 * time.Minute)

	fake := broker.NewFake(domain.Account{
		PortfolioValue: 100_000, Cash: 100_000, Equity: 100_000,
	})
	fake.SetCalendar(broker.CalendarDay{
		Date: scheduler.SessionDate(now), Open: open, Close: close,
	})
	// So the demo exercises the "opens in" countdown too, once its compressed session
	// has finished, rather than showing nothing.
	tomorrow := now.AddDate(0, 0, 1)
	fake.SetNextSession(broker.CalendarDay{
		Date:  scheduler.SessionDate(tomorrow),
		Open:  time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 30, 0, 0, scheduler.ET),
		Close: time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 16, 0, 0, 0, scheduler.ET),
	})

	// A bullish tape so the sentiment gate opens.
	for _, sym := range []string{"SPY", "QQQ", "IWM"} {
		fake.SetSnapshot(sym, 101.10, 100, 5_000_000)
	}

	// A handful of symbols standing in for the tradable universe: one qualifies, one
	// fails on news only, and one never clears the move threshold — so the screening
	// table shows each outcome.
	// Pre-market volume is a separate reading from the snapshot's, because before the
	// bell the snapshot has none — see broker.SessionVolumes. Seeding it is what makes
	// the demo's pre-market phase show a candidate table rather than an empty one.
	fake.SetSnapshot("FLAT", 10.00, 9.95, 800_000)
	fake.SetSessionVolume("FLAT", 400_000)
	fake.SetAverageVolume("FLAT", 750_000)

	fake.SetSnapshot("DEMO", 4.56, 4.00, 6_100_000)
	fake.SetSessionVolume("DEMO", 900_000)
	fake.SetAverageVolume("DEMO", 1_000_000)
	fake.SetNews("DEMO", 2)
	// Clearing the screen is no longer enough to be bought: the chart has to print a
	// pullback and resumption. DEMO gets one, so the demo reaches an actual entry. It
	// has closed by now: pre-market was opened one chart's length ago for it. With
	// pre-market off it starts at the open and is only complete once that many
	// candles have passed.
	chartStart := open
	if cfg.PreMarket.Enabled {
		chartStart = now.Add(-offlineChartLen * cfg.Entry.PatternInterval).Truncate(cfg.Entry.PatternInterval)
	}
	fake.SetSetupBars("DEMO", 4.56, chartStart, cfg.Entry.PatternInterval)

	// SETUPLESS clears every screening criterion but never sets up, which is the
	// common real-world case and the one the page's Action column exists to explain.
	fake.SetSnapshot("SETUPLESS", 3.40, 3.00, 7_000_000)
	fake.SetSessionVolume("SETUPLESS", 1_000_000)
	fake.SetAverageVolume("SETUPLESS", 1_100_000)
	fake.SetNews("SETUPLESS", 1)

	fake.SetSnapshot("NONEWS", 2.30, 2.06, 5_400_000)
	fake.SetSessionVolume("NONEWS", 800_000)
	fake.SetAverageVolume("NONEWS", 1_000_000)
	fake.SetNews("NONEWS", 0)

	return fake
}
