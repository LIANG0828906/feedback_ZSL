"""Fixed-timestep settlement: pure function of (state, operation)."""

from __future__ import annotations

from .errors import BizError, OpOutOfBoundsError, OpUnknownPlayerError
from .model import MAP_BOUND, Operation, RoomState


def apply_operation(state: RoomState, op: Operation):
    """Apply one operation; returns None on success or a BizError.

    Rejections happen before any mutation so a rejected op can never
    silently fork the state.
    """
    entity = state.entities.get(op.player)
    if entity is None:
        return OpUnknownPlayerError("unknown player", player=op.player,
                                    seq=op.seq)
    if op.kind == "move":
        dx = int(op.payload.get("dx", 0))
        dy = int(op.payload.get("dy", 0))
        nx, ny = entity.x + dx, entity.y + dy
        if abs(nx) > MAP_BOUND or abs(ny) > MAP_BOUND:
            return OpOutOfBoundsError("coordinate out of bounds",
                                      player=op.player, seq=op.seq,
                                      x=nx, y=ny, bound=MAP_BOUND)
        entity.x, entity.y = nx, ny
        entity.vx, entity.vy = dx, dy
        return None
    if op.kind == "vel":
        entity.vx = int(op.payload.get("vx", 0))
        entity.vy = int(op.payload.get("vy", 0))
        return None
    if op.kind == "kill":
        entity.alive = False
        return None
    if op.kind == "revive":
        entity.alive = True
        return None
    if op.kind == "resource":
        delta = int(op.payload.get("delta", 0))
        state.resources = max(0, state.resources + delta)
        return None
    return BizError("unknown op kind", kind=op.kind)
