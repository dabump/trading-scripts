// Command agent runs the trading daemon and its status page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
		verbose = flag.Bool("verbose", false, "enable debug logging")
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
		cfg.Exit.EODExitOffsetMins = 15
		logger.Warn("offline mode: session timings compressed",
			"sentiment_window", cfg.Timing.SentimentWindow,
			"scan_interval", cfg.Timing.ScreenerScanInterval)

		fake := seedFake(time.Now())
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
			"position_size_pct":        cfg.Risk.PositionSizePct,
			"max_concurrent_positions": cfg.Risk.MaxConcurrentPositions,
			"stop_loss_pct":            cfg.Risk.StopLossPct,
			"profit_target_pct":        cfg.Exit.ProfitTargetPct,
			"trailing_stop_pct":        cfg.Exit.TrailingStopPct,
			"macd":                     fmt.Sprintf("%d/%d/%d @ %dmin", cfg.Exit.MACDFast, cfg.Exit.MACDSlow, cfg.Exit.MACDSignal, cfg.Exit.MACDIntervalMins),
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

	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(ctx) }()
	go func() { errs <- eng.Run(ctx) }()

	logger.Info("agent started", "paper", paper, "offline", *offline,
		"listen", cfg.Web.ListenAddr, "db", cfg.Storage.DatabasePath,
		"audit", auditLog.Path(scheduler.SessionDate(now)))

	err = <-errs
	if errors.Is(err, context.Canceled) {
		logger.Info("shutting down")
		return nil
	}
	return err
}

// seedFake builds an offline broker with a plausible session and one candidate
// that clears every screening criterion, so the daemon and its page can be
// exercised end to end without credentials.
func seedFake(now time.Time) *broker.Fake {
	// Anchored to now rather than 09:30-16:00 so offline mode is demonstrable
	// whatever the wall clock says. With the compressed timings set by the caller
	// this yields a ~30s sentiment window, ~4.5 minutes of trading, then the EOD
	// window.
	open := now
	close := now.Add(20 * time.Minute)

	fake := broker.NewFake(domain.Account{
		PortfolioValue: 100_000, Cash: 100_000, Equity: 100_000,
	})
	fake.SetCalendar(broker.CalendarDay{
		Date: scheduler.SessionDate(now), Open: open, Close: close,
	})

	// A bullish tape so the sentiment gate opens.
	for _, sym := range []string{"SPY", "QQQ", "IWM"} {
		fake.SetSnapshot(sym, 101.10, 100, 5_000_000)
	}

	// A handful of symbols standing in for the tradable universe: one qualifies, one
	// fails on news only, and one never clears the move threshold — so the screening
	// table shows each outcome.
	fake.SetSnapshot("FLAT", 10.00, 9.95, 800_000)
	fake.SetAverageVolume("FLAT", 750_000)

	fake.SetSnapshot("DEMO", 4.56, 4.00, 6_100_000)
	fake.SetAverageVolume("DEMO", 1_000_000)
	fake.SetNews("DEMO", 2)

	fake.SetSnapshot("NONEWS", 2.30, 2.06, 5_400_000)
	fake.SetAverageVolume("NONEWS", 1_000_000)
	fake.SetNews("NONEWS", 0)

	return fake
}
