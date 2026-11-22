"""Match execution paths and cross-path comparison.

Three execution paths must produce identical results for the same sample,
latency observation and algorithm version:

* ``run_realtime`` — straight live advance (with optional checkpoints)
* ``run_resume``   — cancel partway, reload committed windows, continue
* ``run_replay``   — full recompute from the input stream alone

``compare`` localises the first divergence (tick, unit, field) and raises
DivergeError (E_DIVERGE).
"""

from __future__ import annotations

import copy
import threading
from dataclasses import dataclass

from . import engine
from .checkpoint import CheckpointStore, content_hash
from .client import ClientSim
from .errors import DivergeError, ParamError
from .frames import FramePolicy, FrameRecord, FrameScheduler
from .workers import WindowPool


@dataclass(frozen=True)
class RunConfig:
    window_ticks: int = 30
    render_fps: int = 60
    tick_rate: int = 20
    max_input_delay_frames: int = 3
    correction_window_frames: int = 6
    max_workers: int = 2
    queue_size: int = 4
    result_buffer: int = 16

    def validate(self):
        if self.window_ticks < 1:
            raise ParamError("window_ticks must be >= 1", field="window_ticks")
        FramePolicy(
            render_fps=self.render_fps,
            tick_rate=self.tick_rate,
            max_input_delay_frames=self.max_input_delay_frames,
            correction_window_frames=self.correction_window_frames,
        )
        if self.max_workers < 1 or self.queue_size < 1 or self.result_buffer < 1:
            raise ParamError("worker bounds must be >= 1", field="max_workers")

    @property
    def frame_policy(self):
        return FramePolicy(
            render_fps=self.render_fps,
            tick_rate=self.tick_rate,
            max_input_delay_frames=self.max_input_delay_frames,
            correction_window_frames=self.correction_window_frames,
        )


@dataclass
class RunResult:
    states: list                  # authoritative state per tick (index = tick)
    client_views: dict            # cid -> visible state per tick (+ drain tail)
    frame_log: list               # FrameRecord of the primary client
    corrections: dict             # cid -> [Correction]
    settlement: dict
    status: str = "ok"            # "ok" | "cancelled"


def _latency_table(client):
    return {obs["tick"]: obs["tier"] for obs in client["latency"]}


def _tier_at(table, tick, default=0):
    if tick in table:
        return table[tick]
    earlier = [t for t in table if t <= tick]
    return table[max(earlier)] if earlier else default


def _snapshot(tick, state, history, views, clients, inflight, scheduler):
    auth_core = {
        "tick": tick,
        "input_offset": tick,
        "rng_cursor": tick,
        "state": state,
        "history": history,
    }
    return {
        "tick": tick,
        "input_offset": tick,          # inputs up to this tick are consumed
        "rng_cursor": tick,            # sub-RNG progress == tick (derived)
        "auth_hash": content_hash(auth_core),  # latency-independent identity
        "state": copy.deepcopy(state),
        "history": copy.deepcopy(history),
        "views": copy.deepcopy(views),
        "clients": {cid: c.snapshot() for cid, c in clients.items()},
        "client_acked": {
            cid: (c.confirmed["tick"] if c.confirmed else -1)
            for cid, c in clients.items()
        },
        "inflight": copy.deepcopy(inflight),
        "frame_log": [vars(r) for r in scheduler.log],
        "last_input_frame": scheduler._last_input_frame,
    }


def _encode_and_commit(store, window, snapshot):
    """Worker task: encode (hash) then atomically commit one window."""
    store.commit(window, snapshot)
    return window


def run(sample, rcfg: RunConfig | None = None, ecfg: engine.EngineConfig | None = None,
        *, store: CheckpointStore | None = None, resume: bool = False,
        cancel_event: threading.Event | None = None,
        cancel_after_tick: int | None = None,
        extra_inputs: dict | None = None,
        latency_offset: dict | None = None,
        pool: WindowPool | None = None) -> RunResult:
    """Execute a match.  See module docstring for the three paths."""
    rcfg = rcfg or RunConfig()
    ecfg = ecfg or engine.EngineConfig()
    rcfg.validate()

    inputs_by_tick = {e["tick"]: list(e["intents"]) for e in sample.inputs}
    for tick, intents in (extra_inputs or {}).items():
        inputs_by_tick.setdefault(tick, []).extend(intents)
    max_tick = max(inputs_by_tick) if inputs_by_tick else 0

    clients = {
        c["id"]: ClientSim(c["id"], c["player"], sample.seed, ecfg)
        for c in sample.clients
    }
    latency = {c["id"]: _latency_table(c) for c in sample.clients}
    latency_offset = latency_offset or {}
    player_of = {u["id"]: u["player"] for u in sample.units}
    primary = sample.clients[0]["id"]

    scheduler = FrameScheduler(rcfg.frame_policy)
    inflight = {cid: [] for cid in clients}   # cid -> [(deliver_tick, state)]
    views = {cid: [] for cid in clients}
    frame_no = 0
    start_tick = 0

    if resume and store is not None:
        snapshots = store.load()
        if snapshots:
            snap = snapshots[-1]
            state = copy.deepcopy(snap["state"])
            history = copy.deepcopy(snap["history"])
            views = copy.deepcopy(snap["views"])
            for cid, csnap in snap["clients"].items():
                clients[cid].restore(csnap)
            inflight = copy.deepcopy(snap["inflight"])
            start_tick = snap["tick"]
            scheduler.log = [FrameRecord(**r) for r in snap["frame_log"]]
            frame_no = len(scheduler.log)
            scheduler._last_input_frame = snap.get("last_input_frame")
        else:
            # metadata mismatch -> whole store discarded -> full recompute
            state = engine.initial_state(sample.units)
            history = [copy.deepcopy(state)]
    else:
        state = engine.initial_state(sample.units)
        history = [copy.deepcopy(state)]

    commits = 0
    status = "ok"
    frames_per_tick = rcfg.frame_policy.frames_per_tick

    for tick in range(start_tick + 1, max_tick + 1):
        if cancel_event is not None and cancel_event.is_set():
            status = "cancelled"
            break
        if cancel_after_tick is not None and tick > cancel_after_tick:
            status = "cancelled"
            break

        intents = inputs_by_tick.get(tick, [])
        engine.step(state, intents, sample.seed, ecfg)

        for cid, client in clients.items():
            for intent in intents:
                if player_of.get(intent.get("unit")) == client.player:
                    client.apply_local_input(tick, intent)
                    if cid == primary:
                        scheduler.note_input(frame_no)

        for cid, client in clients.items():
            tier = _tier_at(latency[cid], tick) + latency_offset.get(cid, 0)
            inflight[cid].append((tick + tier, copy.deepcopy(state)))
            due = [d for d in inflight[cid] if d[0] <= tick]
            inflight[cid] = [d for d in inflight[cid] if d[0] > tick]
            for _, auth in sorted(due, key=lambda d: d[0]):
                client.on_authoritative(auth)
                if cid == primary:
                    scheduler.note_correction(frame_no)

        history.append(copy.deepcopy(state))
        for cid, client in clients.items():
            views[cid].append(copy.deepcopy(client.visible_state()))

        for _ in range(frames_per_tick):
            has_unacked = bool(clients[primary].pending)
            scheduler.decide(frame_no, tick, has_unacked)
            frame_no += 1

        if store is not None and tick % rcfg.window_ticks == 0:
            window = tick // rcfg.window_ticks
            snap = _snapshot(tick, state, history, views, clients, inflight,
                             scheduler)
            if pool is not None:
                pool.submit(window, _encode_and_commit, store, window, snap)
                commits += 1
            else:
                store.commit(window, snap)

    # latency drain: deliver every remaining authoritative frame so each
    # client's visible state converges to the authoritative settlement.
    if status == "ok":
        max_latency = max(
            (_tier_at(latency[cid], max_tick) + latency_offset.get(cid, 0))
            for cid in clients
        )
        for extra in range(1, max_latency + 1):
            tick = max_tick + extra
            for cid, client in clients.items():
                due = [d for d in inflight[cid] if d[0] <= tick]
                inflight[cid] = [d for d in inflight[cid] if d[0] > tick]
                for _, auth in sorted(due, key=lambda d: d[0]):
                    client.on_authoritative(auth)
                views[cid].append(copy.deepcopy(client.visible_state()))

    if pool is not None and commits:
        remaining = commits
        while remaining > 0:
            result = pool.poll(timeout=60)
            if result.status != "ok":
                raise RuntimeError(
                    f"checkpoint window {result.window} {result.status}: "
                    f"{result.error}"
                )
            remaining -= 1

    settlement = state["result"] or engine.settlement(state)
    return RunResult(
        states=history,
        client_views=views,
        frame_log=scheduler.log,
        corrections={cid: c.corrections for cid, c in clients.items()},
        settlement=settlement,
        status=status,
    )


def run_realtime(sample, rcfg=None, ecfg=None, **kwargs) -> RunResult:
    return run(sample, rcfg, ecfg, **kwargs)


def run_replay(sample, rcfg=None, ecfg=None, **kwargs) -> RunResult:
    """Full recompute purely from the recorded input stream."""
    kwargs.pop("store", None)
    kwargs.pop("resume", None)
    return run(sample, rcfg, ecfg, store=None, resume=False, **kwargs)


def run_resume(sample, rcfg=None, ecfg=None, *, store, **kwargs) -> RunResult:
    """Continue from the newest committed window in ``store``."""
    return run(sample, rcfg, ecfg, store=store, resume=True, **kwargs)


def _compare_units(tick, units_a, units_b, ctx):
    for uid in sorted(set(units_a) | set(units_b)):
        ua, ub = units_a.get(uid), units_b.get(uid)
        if ua is None or ub is None:
            raise DivergeError(
                f"{ctx}: unit presence diverges", tick=tick, unit=uid,
                field="presence",
            )
        for fname in engine.COMPARE_FIELDS:
            if ua[fname] != ub[fname]:
                raise DivergeError(
                    f"{ctx}: value {ua[fname]!r} != {ub[fname]!r}",
                    tick=tick, unit=uid, field=fname,
                )


def compare(a: RunResult, b: RunResult):
    """Raise DivergeError on the first divergence between two runs."""
    if a.status != b.status:
        raise DivergeError(
            f"status diverges: {a.status} != {b.status}", field="status"
        )
    if len(a.states) != len(b.states):
        raise DivergeError(
            f"tick count diverges: {len(a.states)} != {len(b.states)}",
            tick=min(len(a.states), len(b.states)) - 1, field="tick_count",
        )
    for tick, (sa, sb) in enumerate(zip(a.states, b.states)):
        if sa["locked"] != sb["locked"]:
            raise DivergeError("lock flag diverges", tick=tick, field="locked")
        _compare_units(tick, sa["units"], sb["units"], "authoritative")

    for cid in a.client_views:
        va, vb = a.client_views[cid], b.client_views[cid]
        if len(va) != len(vb):
            raise DivergeError(
                f"client '{cid}' visible sequence length diverges",
                field="client_view", tick=min(len(va), len(vb)),
            )
        for i, (xa, xb) in enumerate(zip(va, vb)):
            if xa is None or xb is None:
                if xa is not xb:
                    raise DivergeError("visible presence diverges", tick=i,
                                       unit=cid, field="client_view")
                continue
            _compare_units(i, xa["units"], xb["units"], f"client '{cid}'")

    if a.settlement != b.settlement:
        raise DivergeError("settlement diverges", field="settlement",
                           tick=a.settlement.get("tick"))

    modes_a = [r.mode for r in a.frame_log]
    modes_b = [r.mode for r in b.frame_log]
    if modes_a != modes_b:
        idx = next(
            (i for i, (x, y) in enumerate(zip(modes_a, modes_b)) if x != y),
            min(len(modes_a), len(modes_b)),
        )
        raise DivergeError("frame mode diverges", tick=idx, field="frame_mode")
