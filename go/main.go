// Command goldcross is a minimal example bot built on tradekit. It fetches
// daily candles for one NSE symbol through the zerodha adapter and looks for
// a simple-moving-average golden cross. In "live" mode (the default) it
// sizes and places a real order through the risk gate chain, records it in
// SQLite, and journals the decision. In "backtest" mode it runs the identical
// crossover rule through core/paper and go/backtest instead, and writes an
// HTML report.
//
// It is deliberately not a strategy worth trading; it exists to show how
// tradekit's pieces fit together.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/indicators"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/harness"
	"github.com/althk/tradekit/go/store"
	"github.com/althk/tradekit/go/zerodha"

	_ "modernc.org/sqlite"
)

const (
	strategyName = "golden_cross"
	orderTag     = "goldxbot"
	// sessionKey is where the day's Kite token is kept between runs, in the
	// store's key-value state table.
	sessionKey = "kite_session"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := run(context.Background()); err != nil {
		slog.Error("goldcross", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Redacted is the one line that's safe to print: APISecret is a
	// harness.Secret field, so it shows as <redacted>.
	slog.Info("config loaded", "config", harness.Redacted(&cfg))

	// The store opens first because the day's token lives in it.
	db, err := store.OpenMigrated(ctx, "sqlite", cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// No access token goes in here — EnsureSession installs one. With no
	// InstrumentToken resolver wired, the client downloads Kite's instrument
	// list on first use and caches it for the process; a bot with a store
	// of instruments would wire store.BrokerID instead.
	client, err := zerodha.New(zerodha.Options{
		APIKey:    cfg.APIKey,
		APISecret: cfg.APISecret.Reveal(),
		Tag:       orderTag,
	})
	if err != nil {
		return err
	}
	// The stored token if Kite still accepts it, otherwise the browser flow
	// on KITE_REDIRECT_URL, persisted for the next run. The timeout is what
	// stops a login nobody completes from hanging a scheduled run.
	err = harness.EnsureSession(ctx, client, db, harness.Session{
		Key:      sessionKey,
		Callback: harness.Callback{RedirectURL: cfg.RedirectURL},
		Timeout:  5 * time.Minute,
	})
	if err != nil {
		return err
	}

	key := domain.InstrumentKey{Exchange: cfg.Exchange, Symbol: cfg.Symbol}
	candles, err := fetchCandles(ctx, client, key, cfg.SlowPeriod)
	if err != nil {
		return err
	}

	switch cfg.Mode {
	case "backtest":
		return runBacktest(ctx, cfg, db, key, candles)
	default:
		return runLive(ctx, cfg, client, db, key, candles)
	}
}

func fetchCandles(ctx context.Context, client *zerodha.Client, key domain.InstrumentKey, slowPeriod int) ([]domain.Candle, error) {
	candles, err := client.Candles(ctx, key, domain.D1, time.Now().AddDate(-1, 0, 0), time.Now())
	if err != nil {
		return nil, fmt.Errorf("fetch candles: %w", err)
	}
	if len(candles) < slowPeriod+2 {
		return nil, fmt.Errorf("only %d candles for %s, need at least %d", len(candles), key, slowPeriod+2)
	}
	return candles, nil
}

// crossover reports whether the fast SMA crossed the slow one between bar
// i-1 and bar i. ok is false while either average is still warming up, and
// at i == 0 where there is no previous bar; both modes share it so live and
// backtest cannot drift on the one rule the example is about.
func crossover(fast, slow []float64, i int) (golden, death, ok bool) {
	if i < 1 || !indicators.IsValid(fast[i-1]) || !indicators.IsValid(slow[i-1]) ||
		!indicators.IsValid(fast[i]) || !indicators.IsValid(slow[i]) {
		return false, false, false
	}
	golden = fast[i-1] <= slow[i-1] && fast[i] > slow[i]
	death = fast[i-1] >= slow[i-1] && fast[i] < slow[i]
	return golden, death, true
}

// chargeTable is a rough, illustrative NSE equity-delivery rate card, shared
// by live's cost estimate and the backtest's paper broker. tradekit ships no
// rate card on purpose (see core/costs's doc comment) — a real bot loads one
// it keeps current, typically from the store via UpsertChargeRate.
func chargeTable(broker string) *costs.Table {
	since := time.Now().AddDate(-1, 0, 0)
	table := costs.NewTable()
	table.Set(broker, costs.EquityDelivery, costs.Brokerage, costs.Rate{Value: 0, EffectiveFrom: since})
	table.Set(broker, costs.EquityDelivery, costs.STTBuy, costs.Rate{Value: 0, EffectiveFrom: since})
	table.Set(broker, costs.EquityDelivery, costs.STTSell, costs.Rate{Value: 0.001, EffectiveFrom: since})
	table.Set(broker, costs.EquityDelivery, costs.Exchange, costs.Rate{Value: 0.0000345, EffectiveFrom: since})
	table.Set(broker, costs.EquityDelivery, costs.SEBI, costs.Rate{Value: 0.000001, EffectiveFrom: since})
	table.Set(broker, costs.EquityDelivery, costs.Stamp, costs.Rate{Value: 0.00015, EffectiveFrom: since})
	table.Set(broker, costs.EquityDelivery, costs.GST, costs.Rate{Value: 0.18, EffectiveFrom: since})
	return table
}

// estimatedCharges logs what one leg would cost at the illustrative rate
// card. Compute prices a round trip, so a single leg is priced as a trip
// that enters and exits at the same price — good enough for a log line.
func estimatedCharges(what string, qty int, price money.Money, buying bool) {
	now := time.Now()
	charges, err := costs.Compute(chargeTable("zerodha"), costs.Trade{
		Broker: "zerodha", Segment: costs.EquityDelivery, Quantity: qty,
		EntryPrice: price, ExitPrice: price, EntryAt: now, ExitAt: now, Buying: buying,
	})
	if err == nil {
		slog.Info("estimated "+what+" charges", harness.Money("total", charges.Total))
	}
}
