"""Example bot: golden-cross entries on one NSE symbol via the Upstox adapter.

Fetches a year of daily candles, looks for a simple-moving-average golden
cross, and if one just happened (and nothing is already held), sizes and
places a market order -- all through tradekit's domain model, risk, costs,
store and harness pieces. It exits the same way on a death cross. This is
deliberately not a strategy worth trading; it exists to show how the pieces
fit together.
"""

from __future__ import annotations

import datetime as dt
import logging
import sys
from dataclasses import dataclass, field

from dotenv import load_dotenv

from tradekit.core import costs, indicators, risk
from tradekit.core import money
from tradekit.core.domain import (
    Candle,
    InstrumentKey,
    OrderRequest,
    OrderType,
    Product,
    Side,
    Signal,
    SignalKind,
    Timeframe,
    TimeInForce,
    held_quantity,
)
from tradekit.harness import Callback, Session, ensure_session, obs
from tradekit.harness import config as harness_config
from tradekit.harness.config import Secret
from tradekit.harness.journal import Decision, Journal
from tradekit.store import Database, connect_migrated
from tradekit.upstox import UpstoxClient

STRATEGY_NAME = "golden_cross"
ORDER_TAG = "goldxbot"

# Where the day's access token is kept between runs, in the store's key-value
# state table.
SESSION_KEY = "upstox_session"
# Where the day's risk counters live, so a second run on the same day still
# knows how many trades the first made.
RISK_STATE_KEY = "risk_daily_state"
# How long a run waits for the browser login; without a bound, a login nobody
# completes leaves a scheduled run hung.
LOGIN_TIMEOUT = 300.0

logger = logging.getLogger("goldcross")


@dataclass(kw_only=True)
class Config:
    """Everything the bot needs.

    Built by `harness.config.overlay`: a field's `env` metadata is read from
    the environment (loaded from `.env` first, see `load_config` below), a
    field with no default is required, and every missing one is reported
    together rather than one restart at a time.
    """

    api_key: str = field(metadata={"env": "UPSTOX_API_KEY"})
    api_secret: Secret = field(metadata={"env": "UPSTOX_API_SECRET"})
    # The redirect registered on the Upstox app; the login callback server
    # listens on its port and path. See `main` below.
    redirect_url: str = field(default="http://127.0.0.1:9880/upstox/callback", metadata={"env": "UPSTOX_REDIRECT_URL"})
    exchange: str = field(default="NSE", metadata={"env": "SYMBOL_EXCHANGE"})
    symbol: str = field(default="RELIANCE", metadata={"env": "SYMBOL"})
    fast_period: int = field(default=20, metadata={"env": "FAST_SMA"})
    slow_period: int = field(default=50, metadata={"env": "SLOW_SMA"})
    stop_pct: float = field(default=0.03, metadata={"env": "STOP_PCT"})
    risk_fraction: float = field(default=0.01, metadata={"env": "RISK_FRACTION"})
    max_trades_per_day: int = field(default=3, metadata={"env": "MAX_TRADES_PER_DAY"})
    db_path: str = field(default="bot.db", metadata={"env": "DB_PATH"})
    # A money.Money field reads a decimal string ("2000.00"); overlay parses
    # it through money.parse, so bare paise are never mistaken for rupees.
    daily_loss_limit: money.Money = field(default=money.parse("2000.00"), metadata={"env": "DAILY_LOSS_LIMIT"})


def load_config() -> Config:
    load_dotenv()  # fine if .env is missing; real environment variables still work
    try:
        return harness_config.overlay({}, Config)
    except ValueError as exc:
        raise SystemExit(f"{exc} (see .env.example)") from None


def crossover(fast: list[float], slow: list[float], i: int) -> tuple[bool, bool, bool]:
    """Whether the fast SMA crossed the slow one between bar i-1 and bar i.

    Returns ``(golden, death, ok)``; ``ok`` is false while either average is
    still warming up, and at ``i == 0`` where there is no previous bar.
    """
    if i < 1 or not all(indicators.is_valid(v) for v in (fast[i - 1], slow[i - 1], fast[i], slow[i])):
        return False, False, False
    golden = fast[i - 1] <= slow[i - 1] and fast[i] > slow[i]
    death = fast[i - 1] >= slow[i - 1] and fast[i] < slow[i]
    return golden, death, True


def estimate_charges(quantity: int, price: money.Money, buying: bool) -> costs.Charges:
    """A rough, illustrative rate card for NSE equity delivery.

    tradekit ships no rate card on purpose (see costs.Table) -- a real bot
    loads one it keeps current, typically from the store via
    `upsert_charge_rate`.
    """
    since = dt.date.today() - dt.timedelta(days=365)
    table = costs.Table()
    table.set("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.BROKERAGE, costs.Rate(value=0, effective_from=since))
    table.set("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_BUY, costs.Rate(value=0, effective_from=since))
    table.set(
        "upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, costs.Rate(value=0.001, effective_from=since)
    )
    table.set(
        "upstox",
        costs.Segment.EQUITY_DELIVERY,
        costs.Kind.EXCHANGE,
        costs.Rate(value=0.0000345, effective_from=since),
    )
    table.set(
        "upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.SEBI, costs.Rate(value=0.000001, effective_from=since)
    )
    table.set(
        "upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STAMP, costs.Rate(value=0.00015, effective_from=since)
    )
    table.set("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.GST, costs.Rate(value=0.18, effective_from=since))

    today = dt.date.today()
    return costs.compute(
        table,
        costs.Trade(
            broker="upstox",
            segment=costs.Segment.EQUITY_DELIVERY,
            quantity=quantity,
            entry_price=price,
            exit_price=price,
            entry_at=today,
            exit_at=today,
            buying=buying,
        ),
    )


def handle_entry(
    client: UpstoxClient,
    db: Database,
    journal: Journal,
    cfg: Config,
    key: InstrumentKey,
    last: Candle,
    run_id: int,
) -> None:
    """Size and place a golden-cross buy.

    The stop is a plain percentage below entry -- this is an example, not a
    strategy -- but sizing, the risk gate chain and the cost estimate are the
    real tradekit pieces.
    """
    entry = last.close
    stop = money.mul_fraction(entry, 1 - cfg.stop_pct)

    sig = Signal(key=key, kind=SignalKind.LONG, at=dt.datetime.now(dt.UTC), price=entry, stop=stop, strategy=STRATEGY_NAME)

    # The day's counters come from the store: each run is one process, and
    # MaxTradesPerDay means nothing if every process starts from zero.
    state = db.load_daily_state(RISK_STATE_KEY, dt.date.today().isoformat())
    tracker = risk.Tracker(state)
    chain = risk.Chain(
        risk.KillSwitch(),
        risk.DailyLossLimit(limit=cfg.daily_loss_limit),
        risk.MaxTradesPerDay(max=cfg.max_trades_per_day),
    )
    try:
        chain.check(sig, tracker.snapshot())
    except risk.RiskBlockedError as exc:
        logger.info("entry blocked by risk gate: %s", exc.reason)
        journal.record(Decision(run_id=run_id, at=dt.datetime.now(dt.UTC), key=key, action="skip", reason=str(exc)))
        return

    account = client.account()
    result = risk.size(
        risk.SizeParams(capital=account.equity, risk_fraction=cfg.risk_fraction, entry=entry, stop=stop, lot_size=1)
    )
    if result.quantity <= 0:
        logger.info("sized to zero: %s", result.reason)
        journal.record(
            Decision(run_id=run_id, at=dt.datetime.now(dt.UTC), key=key, action="skip", reason=result.reason)
        )
        return

    order = client.place_order(
        OrderRequest(
            key=key,
            side=Side.BUY,
            quantity=result.quantity,
            type=OrderType.MARKET,
            product=Product.CNC,
            time_in_force=TimeInForce.DAY,
            tag=ORDER_TAG,
        )
    )
    tracker.record_trade(key)
    # Neither write is fatal: the order is already at the broker, and the
    # next run reconciles from there. What's lost is a row, not money.
    try:
        db.save_daily_state(RISK_STATE_KEY, tracker.snapshot())
        db.record_fill(sig, order, run_id=run_id, paper=False)
    except Exception:
        logger.warning("could not record the fill", exc_info=True)

    charges = estimate_charges(result.quantity, entry, True)
    logger.info("estimated entry charges total=%s", money.format(charges.total))
    logger.info(
        "entered position %s order_id=%s",
        obs.attrs(key=key, side=Side.BUY, qty=result.quantity, entry=entry, stop=stop),
        order.id,
    )

    journal.record(
        Decision(
            run_id=run_id,
            at=dt.datetime.now(dt.UTC),
            key=key,
            action="enter",
            reason="golden cross",
            detail={
                "qty": result.quantity,
                "entry": money.format(entry),
                "stop": money.format(stop),
                "order_id": order.id,
            },
        )
    )


def handle_exit(
    client: UpstoxClient,
    db: Database,
    journal: Journal,
    key: InstrumentKey,
    held: int,
    last: Candle,
    run_id: int,
) -> None:
    """Close the whole position on a death cross."""
    sig = Signal(key=key, kind=SignalKind.EXIT_LONG, at=dt.datetime.now(dt.UTC), price=last.close, strategy=STRATEGY_NAME)

    order = client.place_order(
        OrderRequest(
            key=key,
            side=Side.SELL,
            quantity=held,
            type=OrderType.MARKET,
            product=Product.CNC,
            time_in_force=TimeInForce.DAY,
            tag=ORDER_TAG,
        )
    )

    try:
        db.record_fill(sig, order, run_id=run_id, paper=False)
    except Exception:
        logger.warning("could not record the fill", exc_info=True)

    charges = estimate_charges(held, last.close, False)
    logger.info("estimated exit charges total=%s", money.format(charges.total))
    logger.info("exited position %s order_id=%s", obs.attrs(key=key, side=Side.SELL, qty=held), order.id)

    journal.record(
        Decision(
            run_id=run_id,
            at=dt.datetime.now(dt.UTC),
            key=key,
            action="exit",
            reason="death cross",
            detail={"qty": held, "order_id": order.id},
        )
    )


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    cfg = load_config()
    # The one line that's safe to print: api_secret is a Secret field, so it
    # renders as <redacted> rather than the real value.
    logger.info("config loaded %s", harness_config.redacted(cfg))

    # The store opens first because the day's token lives in it.
    db = connect_migrated(cfg.db_path)

    # No access token goes in here -- ensure_session installs one. With no
    # instrument_key resolver wired, the client downloads Upstox's instrument
    # master on first use and caches it for the process; a bot with a store
    # of instruments would wire db.broker_id instead.
    client = UpstoxClient(
        api_key=cfg.api_key,
        api_secret=cfg.api_secret.reveal(),
        redirect_uri=cfg.redirect_url,
        tag=ORDER_TAG,
    )
    # The stored token if Upstox still accepts it, otherwise the browser flow
    # on redirect_url, persisted for the next run. The timeout is what stops
    # a login nobody completes from hanging a scheduled run.
    ensure_session(client, db, Session(Callback(cfg.redirect_url), key=SESSION_KEY, timeout=LOGIN_TIMEOUT))

    journal = Journal(db)
    key = InstrumentKey(cfg.exchange, cfg.symbol)

    # Bracketed in a store run, so the runs table shows every invocation and
    # how it ended.
    with db.run("live", STRATEGY_NAME) as run_id:
        end = dt.datetime.now(dt.UTC)
        start = end - dt.timedelta(days=400)
        candles = client.candles(key, Timeframe.D1, start, end)
        if len(candles) < cfg.slow_period + 2:
            raise RuntimeError(f"only {len(candles)} candles for {key}, need at least {cfg.slow_period + 2}")

        closes = indicators.closes(candles)
        fast = indicators.sma(closes, cfg.fast_period)
        slow = indicators.sma(closes, cfg.slow_period)

        last = len(candles) - 1
        golden_cross, death_cross, ok = crossover(fast, slow, last)
        if not ok:
            logger.info("indicators still warming up, nothing to do")
            return

        held = held_quantity(client.positions(), key)

        if death_cross and held > 0:
            handle_exit(client, db, journal, key, held, candles[last], run_id)
        elif golden_cross and held == 0:
            handle_entry(client, db, journal, cfg, key, candles[last], run_id)
        else:
            reason = "already in a position" if held > 0 else "no crossover"
            logger.info(
                "no action %s fast_sma=%s slow_sma=%s held=%s",
                obs.attrs(key=key, reason=reason), fast[last], slow[last], held,
            )
            journal.record(
                Decision(
                    run_id=run_id,
                    at=dt.datetime.now(dt.UTC),
                    key=key,
                    action="skip",
                    reason=reason,
                    detail={"fast_sma": fast[last], "slow_sma": slow[last], "held": held},
                )
            )


if __name__ == "__main__":
    try:
        main()
    except SystemExit as exc:
        logger.error(str(exc))
        sys.exit(1)
