"""Client-side prediction with authoritative rollback/replay.

The client keeps a *confirmed* state (the latest authoritative frame it has
received) and a list of *pending* local inputs not yet covered by that
frame.  The predicted (visible) state is the confirmed state with all
pending inputs re-simulated on top.  When an authoritative frame arrives
the client rolls back to it, records the observable correction magnitude,
and replays the still-unconfirmed inputs.
"""

from __future__ import annotations

import copy
from dataclasses import dataclass

from . import engine

PREDICTION_MEMORY = 64  # ticks of predicted snapshots kept for correction diffs


@dataclass
class Correction:
    tick: int
    magnitude: dict
    total: float


class ClientSim:
    def __init__(self, client_id, player, seed, cfg: engine.EngineConfig):
        self.client_id = client_id
        self.player = player
        self.seed = seed
        self.cfg = cfg
        self.confirmed = None          # authoritative state adopted verbatim
        self.pending = []              # [(tick, intent)] own inputs, unconfirmed
        self.predicted = None          # confirmed + pending replayed
        self.corrections = []
        self.rollback_count = 0
        self._pred_snapshots = {}      # tick -> predicted state at that tick

    def apply_local_input(self, tick, intent):
        """Register a local input intent and re-predict immediately."""
        self.pending.append((tick, copy.deepcopy(intent)))
        self._repredict()

    def on_authoritative(self, auth_state):
        """Roll back to the authoritative frame and replay pending inputs."""
        auth_state = copy.deepcopy(auth_state)
        tick = auth_state["tick"]
        prior = self._pred_snapshots.get(tick)
        magnitude = engine.diff_states(prior, auth_state) if prior else {}
        self.corrections.append(
            Correction(tick=tick, magnitude=magnitude,
                       total=engine.diff_total(magnitude))
        )
        self.rollback_count += 1
        self.confirmed = auth_state
        self.pending = [(t, i) for (t, i) in self.pending if t > tick]
        self._repredict()
        self._prune(tick)

    def _repredict(self):
        if self.confirmed is None:
            return
        state = copy.deepcopy(self.confirmed)
        by_tick = {}
        for tick, intent in self.pending:
            by_tick.setdefault(tick, []).append(intent)
        for tick in sorted(by_tick):
            engine.step(state, by_tick[tick], self.seed, self.cfg)
        self.predicted = state
        self._pred_snapshots[state["tick"]] = copy.deepcopy(state)

    def _prune(self, upto_tick):
        stale = [t for t in self._pred_snapshots if t < upto_tick - PREDICTION_MEMORY]
        for t in stale:
            del self._pred_snapshots[t]

    def visible_state(self):
        """What the client renders right now."""
        return self.predicted if self.predicted is not None else self.confirmed

    def snapshot(self):
        return {
            "confirmed": copy.deepcopy(self.confirmed),
            "pending": copy.deepcopy(self.pending),
            "rollback_count": self.rollback_count,
            "corrections": [
                {"tick": c.tick, "magnitude": c.magnitude, "total": c.total}
                for c in self.corrections
            ],
        }

    def restore(self, snap):
        self.confirmed = copy.deepcopy(snap["confirmed"])
        self.pending = copy.deepcopy(snap["pending"])
        self.rollback_count = snap["rollback_count"]
        self.corrections = [
            Correction(tick=c["tick"], magnitude=c["magnitude"], total=c["total"])
            for c in snap["corrections"]
        ]
        self._pred_snapshots = {}
        self._repredict()
