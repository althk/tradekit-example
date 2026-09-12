# tradekit-example

Two minimal "golden cross" bots — one in Go, one in Python — built on
[tradekit](https://github.com/althk/tradekit) to show how its pieces fit
together in a real (if deliberately unambitious) bot: a broker adapter, the
domain model, indicators, risk sizing and gates, cost estimation, SQLite
persistence and the decision journal.

| | Broker | Language |
| --- | --- | --- |
| [`go/`](go/) | Zerodha (Kite Connect) | Go |
| [`python/`](python/) | Upstox | Python |

Both do the same thing: fetch a year of daily candles for one NSE symbol,
compute a fast/slow SMA crossover, and on a golden cross size and place a
market buy (sized off account equity, the stop distance, and a risk gate
chain); on a death cross, sell out. Neither is a strategy worth trading —
there's no confirmation filter, the stop is a flat percentage, and there's no
partial-exit or trailing logic. They exist to be read.

The Go example additionally has a `MODE=backtest` that replays the identical
rule through `core/paper` and `go/backtest` instead of a real broker, and
writes an HTML report — there's no Python equivalent, since tradekit's
`backtest` package isn't mirrored in Python. See `go/README.md` for how the
two modes share code.

## Why bother with risk gates and position sizing at all

A profitable entry signal is not what keeps a trader in the game — surviving
the losing streak every strategy eventually has is. A fixed fraction of
equity risked per trade (`risk.Size`) means a string of losses shrinks the
next position automatically, instead of a bad week compounding into a ruined
account. And a gate chain (`KillSwitch`, `DailyLossLimit`, `MaxTradesPerDay`)
exists for the day the strategy — or the market, or a bug — misbehaves badly
enough that the right response is to stop trading entirely, not to place the
next signal anyway. Skip both and a system that backtests beautifully can
still blow up on the first day reality disagrees with it.

## Prerequisites

Both examples expect to sit next to a checkout of `tradekit` itself:

```text
some-directory/
  tradekit/            # https://github.com/althk/tradekit
  tradekit-example/    # this repo
    go/
    python/
```

tradekit has no tagged releases yet, so both examples resolve it from that
sibling path (`replace` in `go/go.mod`, an editable install in
`python/requirements.txt`). If your layout differs, edit those paths.

Each example needs its own broker credentials — see `go/.env.example` and
`python/.env.example`. Neither example implements the login flow that
produces a daily access token; get one separately (Kite Connect's
`Login`, Upstox's OAuth flow) and paste it in.

## Running

```sh
cd go && cp .env.example .env    # fill in credentials, then:
go run .                         # live
MODE=backtest go run .           # backtest, writes report.html
```

```sh
cd python
python -m venv .venv && .venv\Scripts\Activate.ps1   # or source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env             # fill in credentials, then:
python bot.py
```

See each directory's own README for what it does step by step and which
tradekit package does what.
