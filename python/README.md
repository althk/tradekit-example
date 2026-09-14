# goldcross (Python)

A minimal example bot built on [tradekit](https://github.com/althk/tradekit): it
fetches a year of daily candles for one NSE symbol through the `upstox`
adapter, looks for a simple-moving-average golden cross, and if one just
happened, sizes and places a market order. It exits the same way on a death
cross.

This is **not a strategy worth trading** — there's no confirmation, no
volatility filter, no partial exits. It exists to show how tradekit's pieces
fit together in a single, readable `bot.py`:

| Piece | tradekit package | What it does here |
| --- | --- | --- |
| Domain types | `tradekit.core.domain` | `InstrumentKey`, `Candle`, `OrderRequest`, `Signal` — the vocabulary everything else speaks |
| Money | `tradekit.core.money` | Every price and P&L is an `int` of paise; the stop is `money.mul_fraction(entry, 0.97)` |
| Broker adapter | `tradekit.upstox` | `candles`, `positions`, `account`, `place_order` against the Upstox REST API |
| Indicators | `tradekit.core.indicators` | `sma` over closes, `is_valid` to skip the warm-up period |
| Risk | `tradekit.core.risk` | A gate `Chain` (kill switch, daily loss limit, max trades/day) run before every entry, then `size` computes the quantity from the stop distance |
| Costs | `tradekit.core.costs` | A rough rate card estimates round-trip charges, purely for the log line |
| Persistence | `tradekit.store` | SQLite: every signal and order is recorded, keyed to a run |
| Config | `tradekit.harness.config` | `overlay` builds `Config` from env vars + defaults, aggregates every missing required field into one error, `redacted` prints it without leaking `api_secret` |
| Login | `tradekit.harness.login`, `tradekit.store` state | `browser_login` serves the redirect and has the adapter exchange the code; the token is kept in `kv_state` and reused while `token_fresh` and Upstox still accept it |
| Observability | `tradekit.harness` | `obs.attrs` renders domain values for logs; `Journal` records *why* the bot did (or skipped) something |

There's no Python equivalent of `go/backtest` here — tradekit's Python package
mirrors `core`, `store`, `upstox`, `fyers`, `marketdata` and `harness`, but not
`backtest`. The Go example (`../go/`) has a `MODE=backtest` that replays the
same rule through `core/paper` and `go/backtest`; this one only runs live.

## Setup

```sh
python -m venv .venv
# Windows:
.venv\Scripts\Activate.ps1
# macOS/Linux:
source .venv/bin/activate

pip install -r requirements.txt
cp .env.example .env
# fill in UPSTOX_API_KEY, UPSTOX_API_SECRET; UPSTOX_REDIRECT_URL must match the app

python bot.py
```

`tradekit` isn't published to PyPI or tagged yet, so `requirements.txt`
installs it in editable mode straight from the sibling `../../tradekit/py`
checkout — this repo and `tradekit-example` are expected to sit next to each
other on disk. Point the path at wherever your checkout lives, or switch to a
`git+https://...` requirement once tradekit has tags, if that's not your
layout.

Upstox's tokens expire daily, so `login()` in `bot.py` obtains one on the
first run of the day and keeps it in the store:

1. Reads the last session from the store's key-value state (`db.get_state`).
   If it's from today (`client.token_fresh()`) and Upstox still accepts it
   (`client.account()`), that's the session — a restart mid-day needs no
   browser. A `TokenExpiredError` means a login elsewhere invalidated it.
2. Otherwise calls `browser_login(client, Callback(cfg.redirect_url), timeout=...)`,
   which binds the port from `UPSTOX_REDIRECT_URL`, logs the login URL,
   waits for the redirect, has the adapter exchange the code (`UpstoxClient`
   satisfies `ports.BrowserLogin`) and returns the token. A refused login
   shows a retry link in the browser and keeps waiting; the timeout is what
   stops a login nobody completes from hanging a scheduled run.
3. Stores the token with its issue time (`db.set_state`) for the next run.

The redirect URL must match the one registered on the Upstox developer app
character for character; the default, `http://127.0.0.1:9880/upstox/callback`,
is only a suggestion.

## What happens on a run

1. Loads config from `.env` / the environment (`python-dotenv`).
2. Fetches ~1 year of daily candles for `SYMBOL` on `SYMBOL_EXCHANGE`.
3. Computes a fast and slow SMA (`FAST_SMA`/`SLOW_SMA`, default 20/50).
4. If the fast SMA just crossed above the slow one and nothing is held: runs
   the risk gate chain, sizes the position from `RISK_FRACTION` of account
   equity and the stop distance, places a market buy, and records the signal,
   order and decision.
5. If the fast SMA just crossed below the slow one and a position is held:
   places a market sell to close it out.
6. Otherwise: logs why nothing happened and records that in the journal too.

Every run is one invocation — schedule it (cron, Task Scheduler, whatever) to
run once after the market opens. It is not a long-running process and holds
no state in memory between runs; `bot.db` and the daily risk counters (which
reset by date, not by process) are what make repeated runs behave sensibly.

## Notes

- The stop is a flat 3% (`STOP_PCT`) below entry — not ATR-based, not a swing
  low. Swap it for whatever `tradekit.core.indicators` gives you (`atr`,
  `donchian`, ...) once this stops being a toy.
- The cost estimate in `estimate_charges` is illustrative, not a real rate
  card — see `tradekit.core.costs`'s module docstring for why tradekit ships
  none.
- Upstox addresses cash equity by ISIN, not by exchange+symbol.
  `InstrumentResolver` fetches the instrument master once per run and caches
  it in memory. A real bot would cache this in the store instead
  (`Database.set_broker_id` / `broker_id`).
