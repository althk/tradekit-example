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
fit together, spread across four small files:

| File | tradekit package | What it does here |
| --- | --- | --- |
| `config.go` | `harness` (`Overlay`, `Redacted`) | Env-var config with required-field validation and a startup log line that can't leak a secret |
| `main.go` | `store` (`OpenMigrated`), `zerodha`, `harness` (`EnsureSession`), `core/costs` | Wiring: the SQLite store, the adapter, the daily Kite login, candle fetching, the crossover rule and the illustrative rate card both modes share |
| `live.go` | `core/indicators`, `core/risk`, `store` (`WithRun`, `LoadDailyState`, `RecordFill`), `harness` (`Journal`, `Notifier`, `obs`) | The live path: risk gate chain over persisted daily counters, sizing, order placement, decision journal |
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
through the adapter — and Kite's tokens expire daily, so `main.go` obtains
one on the first run of the day (see below) and keeps it in the store.

## Login: `harness.EnsureSession`

Kite issues a token through a browser redirect: the user visits a login URL,
Kite sends the browser back to the redirect URL registered on the app with a
single-use `request_token`, and the app exchanges it. The bot's part is one
call:

```go
harness.EnsureSession(ctx, client, db, harness.Session{
    Key:      "kite_session",
    Callback: harness.Callback{RedirectURL: cfg.RedirectURL},
    Timeout:  5 * time.Minute,
})
```

which does, in order:

1. Reads the last session from the store's key-value state under `Key`. If
   it's from today (`ports.TokenState`) and Kite still accepts it
   (`client.Account`), that's the session — a restart mid-day needs no
   browser. A token Kite rejects with `ports.ErrTokenExpired` is one that a
   login elsewhere invalidated; anything else is a real error and does not
   burn a browser login.
2. Otherwise runs `harness.BrowserLogin`, which binds the port from
   `KITE_REDIRECT_URL`, logs the login URL, waits for the redirect, has the
   adapter exchange the token (`zerodha.Client` implements
   `ports.BrowserLogin`) and returns it. A refused login shows a retry link
   in the browser and keeps waiting; `Timeout` is what stops a login nobody
   completes from hanging a scheduled run.
3. Stores the token with its issue time for the next run.

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
`money.Money` fields (`DAILY_LOSS_LIMIT`, `BACKTEST_CASH`) are read as
decimal strings — `2000.00`, never bare paise. `harness.Redacted(&cfg)` is
logged once at startup and is the only line that's safe to print —
`APISecret` is a `harness.Secret`, so it renders as `<redacted>`.

## Live mode

1. Fetches ~1 year of daily candles for `SYMBOL` on `SYMBOL_EXCHANGE`.
2. Computes a fast and slow SMA (`FAST_SMA`/`SLOW_SMA`, default 20/50).
3. If the fast SMA just crossed above the slow one and nothing is held: loads
   the day's risk counters from the store (`db.LoadDailyState` — a second run
   on the same day must know what the first did), runs the risk gate chain
   (`KillSwitch`, `DailyLossLimit`, `MaxTradesPerDay`) over them, sizes the
   position from `RISK_FRACTION` of account equity and the stop distance,
   places a market buy, saves the counters back, and records the signal and
   order in one transaction (`db.RecordFill`) plus the decision.
4. If the fast SMA just crossed below the slow one and a position is held:
   places a market sell to close it out.
5. Otherwise: logs why nothing happened and records that in the journal too.

Every run is one invocation, bracketed by `db.WithRun` so the `runs` table
shows each one and how it ended — schedule it (cron, Task Scheduler,
whatever) to run once after the market opens. It is not a long-running
process and holds no state in memory between runs; `bot.db` and the daily
risk counters in it (which reset by date, not by process) are what make
repeated runs behave sensibly.

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
   `report.WriteHTMLFile` writes a self-contained `report.html` (no CDN, no
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
- `zerodha.Options.InstrumentToken` is left nil, so the client downloads
  Kite's instrument list on first use and caches it for the process. A bot
  that keeps instruments in the store would wire `store.BrokerID` instead.
