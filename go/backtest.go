package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/althk/tradekit/go/backtest"
	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/indicators"
	"github.com/althk/tradekit/go/core/paper"
	"github.com/althk/tradekit/go/core/risk"
	"github.com/althk/tradekit/go/harness"
	"github.com/althk/tradekit/go/marketdata"
	"github.com/althk/tradekit/go/store"
)

// runBacktest replays the same golden-cross rule as runLive, bar by bar,
// against core/paper's fill simulator instead of the real broker, then
// writes an HTML report and saves the run into the same store live runs use.
//
// Unlike runLive it does not run the risk gate chain (KillSwitch,
// DailyLossLimit, MaxTradesPerDay): those gates read a *daily* realised P&L,
// and correctly resetting that as simulated days pass is a bit more
// bookkeeping than this example earns its keep for. It still uses risk.Size
// for position sizing, the same as live.
func runBacktest(
	ctx context.Context,
	cfg config,
	db *store.DB,
	key domain.InstrumentKey,
	candles []domain.Candle,
) error {
	broker := paper.New(paper.Options{
		Cash:    cfg.backtestCash,
		Broker:  "zerodha",
		Segment: costs.EquityDelivery,
		Charges: chargeTable("zerodha"),
	})
	clock := backtest.NewSimClock(candles[0].Start)
	replay := &backtest.Replay{Clock: clock, Feed: marketdata.FromSlice(candles), Broker: broker}
	snap := &backtest.Snapshotter{Broker: broker}

	// Golden-cross indices are computed once over the whole history: SMA at
	// bar i only ever looks at bars up to i, so this is exactly as causal as
	// recomputing it fresh inside the loop, just without the O(n^2) cost.
	closes := indicators.Closes(candles)
	fast := indicators.SMA(closes, cfg.FastPeriod)
	slow := indicators.SMA(closes, cfg.SlowPeriod)

	i := -1
	runErr := replay.Run(ctx, func(ctx context.Context, c domain.Candle) error {
		i++
		snap.Observe(c) // after the broker has already applied this bar

		if i == 0 || !indicators.IsValid(fast[i-1]) || !indicators.IsValid(slow[i-1]) ||
			!indicators.IsValid(fast[i]) || !indicators.IsValid(slow[i]) {
			return nil
		}
		goldenCross := fast[i-1] <= slow[i-1] && fast[i] > slow[i]
		deathCross := fast[i-1] >= slow[i-1] && fast[i] < slow[i]

		positions, err := broker.Positions(ctx)
		if err != nil {
			return err
		}
		held := heldQuantity(positions, key)

		switch {
		case deathCross && held > 0:
			_, err := broker.ClosePosition(ctx, key, domain.Buy, c.Close, c.Start, domain.ExitSignal)
			return err
		case goldenCross && held == 0:
			return enterBacktestPosition(ctx, broker, cfg, key, c)
		}
		return nil
	})
	snap.Close() // must run even on error, or the final day's equity point is lost
	if runErr != nil {
		return fmt.Errorf("replay: %w", runErr)
	}

	spec := backtest.RunSpec{
		Name:     "goldcross-backtest",
		Strategy: strategyName,
		From:     candles[0].Start,
		To:       candles[len(candles)-1].Start,
		Opening:  cfg.backtestCash,
		Params: map[string]any{
			"fast_sma": cfg.FastPeriod, "slow_sma": cfg.SlowPeriod,
			"stop_pct": cfg.StopPct, "risk_fraction": cfg.RiskFraction,
		},
	}

	report, err := backtest.Build(spec, broker.Trades(), snap.Points)
	if err != nil {
		return fmt.Errorf("build report: %w", err)
	}

	f, err := os.Create(cfg.ReportPath)
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	defer f.Close()
	if err := report.WriteHTML(f); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	recorder := &backtest.Recorder{DB: db}
	runID, err := recorder.Save(ctx, spec, broker.Trades(), snap.Points)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}

	slog.Info("backtest complete",
		"run_id", runID,
		"trades", report.Metrics.Trades, "wins", report.Metrics.Wins, "losses", report.Metrics.Losses,
		harness.Money("net_pnl", report.Metrics.NetPnL),
		"report", cfg.ReportPath,
	)
	return nil
}

func enterBacktestPosition(ctx context.Context, broker *paper.Broker, cfg config, key domain.InstrumentKey, c domain.Candle) error {
	entry := c.Close
	stop := entry.MulFraction(1 - cfg.StopPct)

	account, err := broker.Account(ctx)
	if err != nil {
		return err
	}
	sizeResult := risk.Size(risk.SizeParams{
		Capital: account.Equity, RiskFraction: cfg.RiskFraction, Entry: entry, Stop: stop, LotSize: 1,
	})
	if sizeResult.Quantity <= 0 {
		return nil
	}

	if _, err := broker.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Buy, Quantity: sizeResult.Quantity,
		Type: domain.Market, Product: domain.CNC, TimeInForce: domain.Day, Tag: orderTag,
	}); err != nil {
		return err
	}
	_, err = broker.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: sizeResult.Quantity, Stop: stop, Product: domain.CNC,
	})
	return err
}
