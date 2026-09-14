# goldcross (Go)

A minimal example bot built on [tradekit](https://github.com/althk/tradekit): it
fetches a year of daily candles for one NSE symbol through the `zerodha`
adapter, looks for a simple-moving-average golden cross, and if one just
happened, sizes and places a market order. It exits the same way on a death
cross. It runs in two modes:

- **`MODE=live`** (default) — places real orders through the `zerodha` adapter.
- **`MODE=backtest`** — replays the identical rule through `core/paper` and
  `go/backtest` instead, and writes an HTML report.

This is **not a strategy worth trading** — there's no confirmation, no
volatility filter, no partial exits. It exists to show how tradekit's pieces
fit together, spread across five small files:

| File | tradekit package | What it does here |
| --- | --- | --- |
| `config.go` | `harness` (`Overlay`, `Redacted`) | Env-var config with required-field validation and a startup log line that can't leak a secret |
| `main.go` | `core/domain`, `core/costs`, `zerodha`, `store` | Wiring: the adapter, the SQLite store, candle fetching, shared helpers |
| `login.go` | `harness` (`BrowserLogin`), `store` (`GetState`/`SetState`), `zerodha` (`TokenFresh`) | The daily Kite login: reuse the stored token if Kite still accepts it, otherwise the browser flow, then persist |
| `live.go` | `core/indicators`, `core/risk`, `core/costs`, `store`, `harness` (`Journal`, `Notifier`, `obs`) | The live path: risk gate chain, sizing, order placement, decision journal |
| `backtest.go` | `core/paper`, `go/backtest`, `core/risk`, `store` | The backtest path: `Replay` drives a paper broker bar by bar, `Snapshotter` marks equity, `Report` renders HTML, `Recorder` saves the run into the same store |

## Setup

```sh
cp .env.example .env
# fill in KITE_API_KEY, KITE_API_SECRET; KITE_REDIRECT_URL must match the app
go run .                      # live
MODE=backtest go run .        # backtest, writes report.html
```

`tradekit` isn't tagged yet, so `go.mod` resolves it from the sibling
`../../tradekit` checkout via `replace` directives — this repo and
`tradekit-example` are expected to sit next to each other on disk. Point them
at a different path, or at a tagged version once one exists, if that's not
your layout.

Both modes need a Kite session — even the backtest fetches real history
through the adapter — and Kite's tokens expire daily, so `login.go` obtains
one on the first run of the day (see below) and keeps it in the store.

## Login: `harness.BrowserLogin`

Kite issues a token through a browser redirect: the user visits a login URL,
Kite sends the browser back to the redirect URL registered on the app with a
single-use `request_token`, and the app exchanges it. `login.go` does the
part that's specific to this bot and leaves the rest to tradekit:

1. Reads the last session from the store's key-value state (`db.GetState`).
   If it's from today (`client.TokenFresh`) and Kite still accepts it
   (`client.Account`), that's the session — a restart mid-day needs no
   browser. A token that Kite rejects with `zerodha.ErrTokenExpired` is one
   that a login elsewhere invalidated; anything else is a real error.
2. Otherwise calls `harness.BrowserLogin(ctx, client, harness.Callback{RedirectURL: ...})`,
   which binds the port from `KITE_REDIRECT_URL`, logs the login URL, waits
   for the redirect, has the adapter exchange the token (`zerodha.Client`
   implements `ports.BrowserLogin`) and returns it. A refused login shows a
   retry link in the browser and keeps waiting; the context's deadline is
   what stops a login nobody completes from hanging a scheduled run.
3. Stores the token with its issue time (`db.SetState`) for the next run.

The redirect URL must match the one registered on the Kite Connect developer
console character for character; the default, `http://127.0.0.1:9880/kite/callback`,
is only a suggestion.

## Config: `harness.Overlay`

`config.go` doesn't read the environment itself — it declares a struct with
`env:"..."` and `validate:"required"` tags and lets `harness.Overlay` do it,
the same mechanism `go/harness`'s own tests use:

```go
type config struct {
    APIKey      string         `env:"KITE_API_KEY" validate:"required"`
    APISecret   harness.Secret `env:"KITE_API_SECRET" validate:"required"`
    RedirectURL string         `env:"KITE_REDIRECT_URL"`
    // ...
}
```

Two missing credentials produce one error naming both, not two restarts.
`harness.Redacted(&cfg)` is logged once at startup and is the only line
that's safe to print — `APISecret` is a `harness.Secret`, so it renders as
`<redacted>`.

## Live mode

1. Fetches ~1 year of daily candles for `SYMBOL` on `SYMBOL_EXCHANGE`.
2. Computes a fast and slow SMA (`FAST_SMA`/`SLOW_SMA`, default 20/50).
3. If the fast SMA just crossed above the slow one and nothing is held: runs
   the risk gate chain (`KillSwitch`, `DailyLossLimit`, `MaxTradesPerDay`),
   sizes the position from `RISK_FRACTION` of account equity and the stop
   distance, places a market buy, and records the signal, order and decision.
4. If the fast SMA just crossed below the slow one and a position is held:
   places a market sell to close it out.
5. Otherwise: logs why nothing happened and records that in the journal too.

Every run is one invocation — schedule it (cron, Task Scheduler, whatever) to
run once after the market opens. It is not a long-running process and holds
no state in memory between runs; `bot.db` and the daily risk counters (which
reset by date, not by process) are what make repeated runs behave sensibly.

A `harness.Notifier` is wired in as `harness.Discard{}` — a one-line no-op.
Swap it for `harness.NewTelegram(token, chatID)` to get paged on entries,
exits and risk refusals instead; nothing else in `live.go` changes.

## Backtest mode

Same candles, same crossover rule, but instead of `client.PlaceOrder` it
calls `broker.PlaceOrder` on a `core/paper` simulator, driven bar by bar by
`backtest.Replay`:

1. `backtest.NewSimClock` + `marketdata.FromSlice(candles)` + a `paper.Broker`
   (seeded with `BACKTEST_CASH` and the same illustrative rate card live's
   cost estimate uses) feed a `backtest.Replay`.
2. Its `Run` loop hands each bar to the broker first — so stops and targets
   already resolved by the time the strategy callback sees the bar — then
   calls back into the same golden/death-cross logic as live, sized the same
   way via `risk.Size`.
3. A `backtest.Snapshotter` marks equity to market once per bar.
4. `backtest.Build` turns the trades and equity curve into a `Report`;
   `report.WriteHTML` writes a self-contained `report.html` (no CDN, no
   script — open it directly in a browser).
5. `backtest.Recorder.Save` writes the run, trades and equity curve into the
   same `bot.db` live runs use, so `db.Trades`/`db.EquityCurve` see backtest
   and live runs side by side.

The backtest **doesn't** run the risk gate chain live mode does: `KillSwitch`,
`DailyLossLimit` and `MaxTradesPerDay` all read a state that resets by
*calendar date*, and correctly rolling that forward as simulated days pass is
more bookkeeping than this example earns its keep for. It still uses
`risk.Size` for position sizing, same as live — sizing is the part that
matters for realism; the day-boundary gates are what's cut.

## Notes

- The stop is a flat 3% (`STOP_PCT`) below entry — not ATR-based, not a swing
  low. Swap it for whatever `core/indicators` gives you (`ATR`, `Donchian`,
  ...) once this stops being a toy.
- The cost estimate (`chargeTable` in `main.go`) is illustrative, not a real
  rate card — see `core/costs`'s doc comment for why tradekit ships none. The
  backtest actually deducts these charges per trade (`paper.Options.Charges`);
  live only logs an estimate, since the real broker's contract note is the
  source of truth there.
- `InstrumentToken` is resolved lazily via `Client.InstrumentTokens`, cached
  for the process's lifetime. A real bot would cache this in the store
  instead (`store.SetBrokerID` / `BrokerID`).
