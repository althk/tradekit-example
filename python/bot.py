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
    Position,
    Product,
    Side,
    Signal,
    SignalKind,
    Timeframe,
    TimeInForce,
)
from tradekit.harness import config as harness_config
from tradekit.harness import obs
from tradekit.harness.config import Secret
from tradekit.harness.journal import Decision, Journal
from tradekit.store import connect
from tradekit.upstox import UpstoxClient
from tradekit.upstox.mapping import instrument_key as build_instrument_key

STRATEGY_NAME = "golden_cross"
ORDER_TAG = "goldxbot"

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
    access_token: Secret = field(metadata={"env": "UPSTOX_ACCESS_TOKEN"})
    exchange: str = field(default="NSE", metadata={"env": "SYMBOL_EXCHANGE"})
    symbol: str = field(default="RELIANCE", metadata={"env": "SYMBOL"})
    fast_period: int = field(default=20, metadata={"env": "FAST_SMA"})
    slow_period: int = field(default=50, metadata={"env": "SLOW_SMA"})
    stop_pct: float = field(default=0.03, metadata={"env": "STOP_PCT"})
    risk_fraction: float = field(default=0.01, metadata={"env": "RISK_FRACTION"})
    max_trades_per_day: int = field(default=3, metadata={"env": "MAX_TRADES_PER_DAY"})
    db_path: str = field(default="bot.db", metadata={"env": "DB_PATH"})
    # money.Money isn't one of harness.config's overlay types (it reads a bare
    # decimal string, not paise), so this is read as a string and parsed below.
    daily_loss_limit_raw: str = field(default="2000.00", metadata={"env": "DAILY_LOSS_LIMIT"})


def load_config() -> Config:
    load_dotenv()  # fine if .env is missing; real environment variables still work
    try:
        cfg = harness_config.overlay({}, Config)
    except ValueError as exc:
        raise SystemExit(f"{exc} (see .env.example)") from None
    # Not a dataclass field: harness.config.overlay() would try to pass it to
    # Config's constructor, and a plain attribute set afterward is simpler
    # than an init=False field it would have to special-case around.
    cfg.daily_loss_limit = money.parse(cfg.daily_loss_limit_raw)
    return cfg


class InstrumentResolver:
    """Resolves a domain InstrumentKey to Upstox's SEGMENT|ISIN key, lazily.

    Upstox addresses cash equity by ISIN, which the adapter cannot derive from
    an exchange and a symbol. The client needs this resolver before it can
    exist, and the resolver needs the client to fetch the instrument master --
    so `client` is wired in after construction, on first use.
    """

    def __init__(self) -> None:
        self.client: UpstoxClient | None = None
        self._isin_by_key: dict[InstrumentKey, str] = {}

    def __call__(self, key: InstrumentKey) -> str:
        if not self._isin_by_key:
            assert self.client is not None, "InstrumentResolver.client must be set before use"
            for instrument in self.client.instruments(key.exchange):
                self._isin_by_key[instrument.key] = instrument.isin
        isin = self._isin_by_key.get(key)
        if not isin:
            raise RuntimeError(f"no ISIN found for {key}; check SYMBOL / SYMBOL_EXCHANGE")
        return build_instrument_key(key, isin)


def held_quantity(positions: list[Position], key: InstrumentKey) -> int:
    for p in positions:
        if p.key == key and p.quantity > 0:
            return p.quantity
    return 0


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
    db,
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

    state = risk.DailyState(date=dt.date.today().isoformat())
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

    db.insert_signal(sig, run_id=run_id, paper=False)
    db.upsert_order(order, paper=False)

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
    db,
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

    db.insert_signal(sig, run_id=run_id, paper=False)
    db.upsert_order(order, paper=False)

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
    # The one line that's safe to print: access_token is a Secret field, so it
    # renders as <redacted> rather than the real value.
    logger.info("config loaded %s", harness_config.redacted(cfg))

    resolver = InstrumentResolver()
    client = UpstoxClient(
        api_key=cfg.api_key,
        access_token=cfg.access_token.reveal(),
        token_issued_at=dt.datetime.now(dt.UTC),
        instrument_key=resolver,
        tag=ORDER_TAG,
    )
    resolver.client = client

    db = connect(cfg.db_path)
    db.migrate()
    journal = Journal(db)
    run_id = db.start_run("live", STRATEGY_NAME, None)

    key = InstrumentKey(cfg.exchange, cfg.symbol)

    try:
        end = dt.datetime.now(dt.UTC)
        start = end - dt.timedelta(days=400)
        candles = client.candles(key, Timeframe.D1, start, end)
        if len(candles) < cfg.slow_period + 2:
            raise RuntimeError(f"only {len(candles)} candles for {key}, need at least {cfg.slow_period + 2}")

        closes = indicators.closes(candles)
        fast = indicators.sma(closes, cfg.fast_period)
        slow = indicators.sma(closes, cfg.slow_period)

        last, prev = len(candles) - 1, len(candles) - 2
        if not all(indicators.is_valid(v) for v in (fast[prev], slow[prev], fast[last], slow[last])):
            logger.info("indicators still warming up, nothing to do")
            db.finish_run(run_id, "ok", "warming up")
            return

        golden_cross = fast[prev] <= slow[prev] and fast[last] > slow[last]
        death_cross = fast[prev] >= slow[prev] and fast[last] < slow[last]

        positions = client.positions()
        held = held_quantity(positions, key)

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
    except Exception as exc:
        db.finish_run(run_id, "error", str(exc))
        raise
    else:
        db.finish_run(run_id, "ok", "")


if __name__ == "__main__":
    try:
        main()
    except SystemExit as exc:
        logger.error(str(exc))
        sys.exit(1)
