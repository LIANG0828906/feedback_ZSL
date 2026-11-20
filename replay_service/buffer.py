"""Per-player operation buffer with dedupe and late-window rejection."""

from __future__ import annotations

from .errors import OpDuplicateError, OpLateError
from .model import Operation


class OpBuffer:
    """Buffers operations by arrival order; drains them per step boundary.

    - Duplicate (player, seq) keeps only the first arrival.
    - Operations older than ``late_window`` steps are rejected with
      E_OP_LATE and recorded in the event sink.
    """

    def __init__(self, late_window: int):
        self.late_window = int(late_window)
        self._pending = []            # list[Operation], arrival order
        self._seen = set()            # (player, seq) offered this session
        self._applied = {}            # player -> set(seq) already settled

    def load_watermark(self, applied: dict) -> None:
        self._applied = {p: set(seqs) for p, seqs in applied.items()}

    def offer(self, op: Operation, current_step: int, events: list) -> bool:
        if op.tick < current_step - self.late_window:
            err = OpLateError("operation beyond late window",
                              player=op.player, seq=op.seq, tick=op.tick,
                              current_step=current_step,
                              late_window=self.late_window)
            events.append({"step": current_step, **err.to_dict()})
            return False
        key = (op.player, op.seq)
        if key in self._seen or op.seq in self._applied.get(op.player, ()):
            err = OpDuplicateError("duplicate client sequence, first kept",
                                   player=op.player, seq=op.seq,
                                   tick=op.tick, current_step=current_step)
            events.append({"step": current_step, **err.to_dict()})
            return False
        self._seen.add(key)
        self._pending.append(op)
        return True

    def drain(self, step: int) -> list:
        """Stable order: (tick, arrival). Buffer is empty after a boundary."""
        ready = [op for op in self._pending if op.tick <= step]
        self._pending = [op for op in self._pending if op.tick > step]
        ready.sort(key=lambda o: (o.tick, o.arrival))
        return ready

    def mark_applied(self, op: Operation) -> None:
        self._applied.setdefault(op.player, set()).add(op.seq)

    def watermark(self) -> dict:
        return {p: sorted(seqs) for p, seqs in sorted(self._applied.items())}
