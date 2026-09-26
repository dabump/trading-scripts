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

	var (
		data           broker.MarketData
		trading        broker.Trading
		floats         broker.FloatProvider = broker.NoFloatProvider{}
		paper                               = true
		floatAvailable                      = false
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
		data, trading, floats = fake, fake, fake
		floatAvailable = true
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

		alpaca := broker.NewAlpaca(secrets)
		data, trading = alpaca, alpaca
		// float_provider is validated to "none", so nothing supplies float data yet
		// and the screener fails that criterion closed.
		logger.Warn("no float data source is configured; the float criterion will fail " +
			"for every candidate and nothing can qualify (see docs/decisions.md)")
	}

	eng := engine.New(engine.Deps{
		Config: cfg, Store: st, Data: data, Trading: trading, Floats: floats,
		Logger: logger,
	})

	srv, err := web.NewServer(cfg, st, eng, logger, time.Now, paper, floatAvailable)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(ctx) }()
	go func() { errs <- eng.Run(ctx) }()

	logger.Info("agent started", "paper", paper, "offline", *offline,
		"listen", cfg.Web.ListenAddr, "db", cfg.Storage.DatabasePath)

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

	// One qualifying low-float mover, and one that fails on news only, so the
	// screening table shows both outcomes.
	fake.SetMovers(
		broker.Mover{Symbol: "DEMO", Price: 4.56, ChangePct: 14},
		broker.Mover{Symbol: "NONEWS", Price: 2.30, ChangePct: 11.5},
	)
	fake.SetSnapshot("DEMO", 4.56, 4.00, 6_100_000)
	fake.SetAverageVolume("DEMO", 1_000_000)
	fake.SetNews("DEMO", 2)
	fake.SetFloat("DEMO", 4_200_000)

	fake.SetSnapshot("NONEWS", 2.30, 2.06, 5_400_000)
	fake.SetAverageVolume("NONEWS", 1_000_000)
	fake.SetNews("NONEWS", 0)
	fake.SetFloat("NONEWS", 8_900_000)

	return fake
}
