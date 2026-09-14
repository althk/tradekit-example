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
	"github.com/althk/tradekit/go/harness"
	"github.com/althk/tradekit/go/store"
	"github.com/althk/tradekit/go/zerodha"

	_ "modernc.org/sqlite"
)

const (
	strategyName = "golden_cross"
	orderTag     = "goldxbot"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	// Redacted is the one line that's safe to print: APISecret is a
	// harness.Secret field, so it shows as <redacted>.
	slog.Info("config loaded", "config", harness.Redacted(&cfg))

	ctx := context.Background()

	db, err := store.Open("sqlite", cfg.DBPath)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		slog.Error("migrate store", "err", err)
		os.Exit(1)
	}

	// The store opens first because the day's token lives in it; see login.go.
	client, err := newZerodhaClient(ctx, cfg)
	if err != nil {
		slog.Error("zerodha client", "err", err)
		os.Exit(1)
	}
	if err := login(ctx, client, db, cfg); err != nil {
		slog.Error("login", "err", err)
		os.Exit(1)
	}

	key := domain.InstrumentKey{Exchange: cfg.Exchange, Symbol: cfg.Symbol}
	candles, err := fetchCandles(ctx, client, key, cfg.SlowPeriod)
	if err != nil {
		slog.Error("fetch candles", "err", err)
		os.Exit(1)
	}

	switch cfg.Mode {
	case "backtest":
		err = runBacktest(ctx, cfg, db, key, candles)
	default:
		err = runLive(ctx, cfg, client, db, key, candles)
	}
	if err != nil {
		slog.Error("run", "mode", cfg.Mode, "err", err)
		os.Exit(1)
	}
}

// newZerodhaClient wires up the adapter with a lazy instrument-token
// resolver: Kite's historical endpoint needs its own numeric token, which
// nothing about an exchange+symbol pair reveals, so it's fetched once, on
// first use, and cached for the process's lifetime. No access token goes in
// here — login installs one.
func newZerodhaClient(ctx context.Context, cfg config) (*zerodha.Client, error) {
	var client *zerodha.Client
	var tokens map[domain.InstrumentKey]int

	lookupToken := func(k domain.InstrumentKey) (int, error) {
		if tokens == nil {
			var err error
			tokens, err = client.InstrumentTokens(ctx, k.Exchange)
			if err != nil {
				return 0, fmt.Errorf("fetch instrument tokens: %w", err)
			}
		}
		t, ok := tokens[k]
		if !ok {
			return 0, fmt.Errorf("no instrument token for %s", k)
		}
		return t, nil
	}

	var err error
	client, err = zerodha.New(zerodha.Options{
		APIKey:          cfg.APIKey,
		APISecret:       cfg.APISecret.Reveal(),
		InstrumentToken: lookupToken,
		Tag:             orderTag,
	})
	return client, err
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

func heldQuantity(positions []domain.Position, key domain.InstrumentKey) int {
	for _, p := range positions {
		if p.Key == key && p.Quantity > 0 {
			return p.Quantity
		}
	}
	return 0
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
