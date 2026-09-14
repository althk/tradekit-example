package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/indicators"
	"github.com/althk/tradekit/go/core/risk"
	"github.com/althk/tradekit/go/harness"
	"github.com/althk/tradekit/go/store"
	"github.com/althk/tradekit/go/zerodha"
)

// riskStateKey is where the day's risk counters live in the store, so a
// second run on the same day still knows how many trades the first made.
const riskStateKey = "risk_daily_state"

// runLive checks for a crossover on the latest candle and, if one just
// happened, places a real order. The whole thing is bracketed in a store
// run, so the runs table shows every invocation and how it ended.
func runLive(
	ctx context.Context,
	cfg config,
	client *zerodha.Client,
	db *store.DB,
	key domain.InstrumentKey,
	candles []domain.Candle,
) error {
	return db.WithRun(ctx, "live", strategyName, nil, func(ctx context.Context, runID int64) error {
		return decide(ctx, cfg, client, db, key, candles, runID)
	})
}

func decide(
	ctx context.Context,
	cfg config,
	client *zerodha.Client,
	db *store.DB,
	key domain.InstrumentKey,
	candles []domain.Candle,
	runID int64,
) error {
	journal := &harness.Journal{DB: db}
	// Discard sends nothing anywhere; swap in harness.NewTelegram(cfg.TelegramToken,
	// cfg.ChatID) to get paged on entries, exits and risk refusals instead.
	var notifier harness.Notifier = harness.Discard{}

	closes := indicators.Closes(candles)
	fast := indicators.SMA(closes, cfg.FastPeriod)
	slow := indicators.SMA(closes, cfg.SlowPeriod)

	last := len(candles) - 1
	goldenCross, deathCross, ok := crossover(fast, slow, last)
	if !ok {
		slog.Info("indicators still warming up, nothing to do")
		return nil
	}

	positions, err := client.Positions(ctx)
	if err != nil {
		return fmt.Errorf("fetch positions: %w", err)
	}
	held := domain.HeldQuantity(positions, key)

	switch {
	case deathCross && held > 0:
		return handleExit(ctx, client, db, journal, notifier, key, held, candles[last], runID)
	case goldenCross && held == 0:
		return handleEntry(ctx, client, db, journal, notifier, cfg, key, candles[last], runID)
	default:
		reason := "no crossover"
		if held > 0 {
			reason = "already in a position"
		}
		slog.Info("no action", harness.Key(key), harness.Reason(reason),
			"fast_sma", fast[last], "slow_sma", slow[last], "held", held)
		return journal.Record(ctx, harness.Decision{
			RunID: runID, At: time.Now(), Key: key, Action: "skip", Reason: reason,
			Detail: map[string]any{"fast_sma": fast[last], "slow_sma": slow[last], "held": held},
		})
	}
}

// handleEntry sizes and places a golden-cross buy. The stop is a plain
// percentage below entry — this is an example, not a strategy — but sizing,
// the risk gate chain and the cost estimate are the real tradekit pieces.
func handleEntry(
	ctx context.Context,
	client *zerodha.Client,
	db *store.DB,
	journal *harness.Journal,
	notifier harness.Notifier,
	cfg config,
	key domain.InstrumentKey,
	last domain.Candle,
	runID int64,
) error {
	entry := last.Close
	stop := entry.MulFraction(1 - cfg.StopPct)

	sig := domain.Signal{
		Key: key, Kind: domain.Long, At: time.Now(),
		Price: entry, Stop: stop, Strategy: strategyName,
	}

	// The day's counters come from the store: each run is one process, and
	// MaxTradesPerDay means nothing if every process starts from zero.
	state, err := db.LoadDailyState(ctx, riskStateKey, time.Now().Format("2006-01-02"))
	if err != nil {
		return err
	}
	tracker := risk.NewTracker(state)
	chain := risk.Chain{
		risk.KillSwitch{},
		risk.DailyLossLimit{Limit: cfg.DailyLossLimit},
		risk.MaxTradesPerDay{Max: cfg.MaxTrades},
	}
	if err := chain.Check(ctx, sig, tracker.Snapshot()); err != nil {
		slog.Info("entry blocked by risk gate", harness.Key(key), harness.Reason(err.Error()))
		notifier.Notify(ctx, harness.Warn, fmt.Sprintf("%s entry blocked: %s", key, err))
		return journal.Record(ctx, harness.Decision{
			RunID: runID, At: time.Now(), Key: key, Action: "skip", Reason: err.Error(),
		})
	}

	account, err := client.Account(ctx)
	if err != nil {
		return fmt.Errorf("fetch account: %w", err)
	}

	sizeResult := risk.Size(risk.SizeParams{
		Capital:      account.Equity,
		RiskFraction: cfg.RiskFraction,
		Entry:        entry,
		Stop:         stop,
		LotSize:      1,
	})
	if sizeResult.Quantity <= 0 {
		slog.Info("sized to zero", harness.Key(key), harness.Reason(sizeResult.Reason))
		return journal.Record(ctx, harness.Decision{
			RunID: runID, At: time.Now(), Key: key, Action: "skip", Reason: sizeResult.Reason,
		})
	}

	order, err := client.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Buy, Quantity: sizeResult.Quantity,
		Type: domain.Market, Product: domain.CNC, TimeInForce: domain.Day,
		Tag: orderTag,
	})
	if err != nil {
		return fmt.Errorf("place order: %w", err)
	}
	tracker.RecordTrade(key)
	// Neither write is fatal: the order is already at the broker, and the
	// next run reconciles from there. What's lost is a row, not money.
	if err := db.SaveDailyState(ctx, riskStateKey, tracker.Snapshot()); err != nil {
		slog.Warn("save risk state", "err", err)
	}
	if _, err := db.RecordFill(ctx, sig, order, runID, false); err != nil {
		slog.Warn("record fill", "err", err)
	}
	estimatedCharges("entry", sizeResult.Quantity, entry, true)

	slog.Info("entered position", harness.Key(key), harness.Side(domain.Buy), harness.Qty(sizeResult.Quantity),
		harness.Money("entry", entry), harness.Money("stop", stop), "order_id", order.ID)
	notifier.Notify(ctx, harness.Info, fmt.Sprintf("entered %s qty=%d entry=%s stop=%s",
		key, sizeResult.Quantity, entry, stop))

	return journal.Record(ctx, harness.Decision{
		RunID: runID, At: time.Now(), Key: key, Action: "enter", Reason: "golden cross",
		Detail: map[string]any{"qty": sizeResult.Quantity, "entry": entry.String(), "stop": stop.String(), "order_id": order.ID},
	})
}

// handleExit closes the whole position on a death cross.
func handleExit(
	ctx context.Context,
	client *zerodha.Client,
	db *store.DB,
	journal *harness.Journal,
	notifier harness.Notifier,
	key domain.InstrumentKey,
	held int,
	last domain.Candle,
	runID int64,
) error {
	sig := domain.Signal{Key: key, Kind: domain.ExitLong, At: time.Now(), Price: last.Close, Strategy: strategyName}

	order, err := client.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Sell, Quantity: held,
		Type: domain.Market, Product: domain.CNC, TimeInForce: domain.Day,
		Tag: orderTag,
	})
	if err != nil {
		return fmt.Errorf("place exit order: %w", err)
	}
	if _, err := db.RecordFill(ctx, sig, order, runID, false); err != nil {
		slog.Warn("record fill", "err", err)
	}
	estimatedCharges("exit", held, last.Close, false)

	slog.Info("exited position", harness.Key(key), harness.Side(domain.Sell), harness.Qty(held), "order_id", order.ID)
	notifier.Notify(ctx, harness.Info, fmt.Sprintf("exited %s qty=%d", key, held))

	return journal.Record(ctx, harness.Decision{
		RunID: runID, At: time.Now(), Key: key, Action: "exit", Reason: "death cross",
		Detail: map[string]any{"qty": held, "order_id": order.ID},
	})
}
