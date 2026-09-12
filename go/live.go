package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/indicators"
	"github.com/althk/tradekit/go/core/risk"
	"github.com/althk/tradekit/go/harness"
	"github.com/althk/tradekit/go/store"
	"github.com/althk/tradekit/go/zerodha"
)

// runLive checks for a crossover on the latest candle and, if one just
// happened, places a real order.
func runLive(
	ctx context.Context,
	cfg config,
	client *zerodha.Client,
	db *store.DB,
	key domain.InstrumentKey,
	candles []domain.Candle,
) error {
	journal := &harness.Journal{DB: db}
	// Discard sends nothing anywhere; swap in harness.NewTelegram(cfg.TelegramToken,
	// cfg.ChatID) to get paged on entries, exits and risk refusals instead.
	var notifier harness.Notifier = harness.Discard{}

	runID, err := db.StartRun(ctx, "live", strategyName, nil)
	if err != nil {
		return fmt.Errorf("start run: %w", err)
	}

	closes := indicators.Closes(candles)
	fast := indicators.SMA(closes, cfg.FastPeriod)
	slow := indicators.SMA(closes, cfg.SlowPeriod)

	last := len(candles) - 1
	prev := last - 1
	if !indicators.IsValid(fast[prev]) || !indicators.IsValid(slow[prev]) ||
		!indicators.IsValid(fast[last]) || !indicators.IsValid(slow[last]) {
		slog.Info("indicators still warming up, nothing to do")
		return db.FinishRun(ctx, runID, "ok", "warming up")
	}

	goldenCross := fast[prev] <= slow[prev] && fast[last] > slow[last]
	deathCross := fast[prev] >= slow[prev] && fast[last] < slow[last]

	positions, err := client.Positions(ctx)
	if err != nil {
		_ = db.FinishRun(ctx, runID, "error", err.Error())
		return fmt.Errorf("fetch positions: %w", err)
	}
	held := heldQuantity(positions, key)

	switch {
	case deathCross && held > 0:
		err = handleExit(ctx, client, db, journal, notifier, key, held, candles[last], runID)
	case goldenCross && held == 0:
		err = handleEntry(ctx, client, db, journal, notifier, cfg, key, candles[last], runID)
	default:
		reason := "no crossover"
		if held > 0 {
			reason = "already in a position"
		}
		slog.Info("no action", harness.Key(key), harness.Reason(reason),
			"fast_sma", fast[last], "slow_sma", slow[last], "held", held)
		err = journal.Record(ctx, harness.Decision{
			RunID: runID, At: time.Now(), Key: key, Action: "skip", Reason: reason,
			Detail: map[string]any{"fast_sma": fast[last], "slow_sma": slow[last], "held": held},
		})
	}
	if err != nil {
		_ = db.FinishRun(ctx, runID, "error", err.Error())
		return err
	}

	return db.FinishRun(ctx, runID, "ok", "")
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

	state := risk.NewDailyState(time.Now().Format("2006-01-02"))
	tracker := risk.NewTracker(state)
	chain := risk.Chain{
		risk.KillSwitch{},
		risk.DailyLossLimit{Limit: cfg.dailyLossCap},
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

	if _, err := db.InsertSignal(ctx, sig, runID, false); err != nil {
		slog.Warn("record signal", "err", err)
	}
	if err := db.UpsertOrder(ctx, order, false); err != nil {
		slog.Warn("record order", "err", err)
	}

	charges, err := costs.Compute(chargeTable("zerodha"), costs.Trade{
		Broker: "zerodha", Segment: costs.EquityDelivery, Quantity: sizeResult.Quantity,
		EntryPrice: entry, ExitPrice: entry, EntryAt: time.Now(), ExitAt: time.Now(), Buying: true,
	})
	if err == nil {
		slog.Info("estimated entry charges", harness.Money("total", charges.Total))
	}

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

	if _, err := db.InsertSignal(ctx, sig, runID, false); err != nil {
		slog.Warn("record signal", "err", err)
	}
	if err := db.UpsertOrder(ctx, order, false); err != nil {
		slog.Warn("record order", "err", err)
	}

	charges, err := costs.Compute(chargeTable("zerodha"), costs.Trade{
		Broker: "zerodha", Segment: costs.EquityDelivery, Quantity: held,
		EntryPrice: last.Close, ExitPrice: last.Close, EntryAt: time.Now(), ExitAt: time.Now(), Buying: false,
	})
	if err == nil {
		slog.Info("estimated exit charges", harness.Money("total", charges.Total))
	}

	slog.Info("exited position", harness.Key(key), harness.Side(domain.Sell), harness.Qty(held), "order_id", order.ID)
	notifier.Notify(ctx, harness.Info, fmt.Sprintf("exited %s qty=%d", key, held))

	return journal.Record(ctx, harness.Decision{
		RunID: runID, At: time.Now(), Key: key, Action: "exit", Reason: "death cross",
		Detail: map[string]any{"qty": held, "order_id": order.ID},
	})
}
