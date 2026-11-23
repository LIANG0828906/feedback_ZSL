"""Unit tactical advance along a smoothed polyline (fixed-point).

The unit's position is a pure function of the travelled arc length s on
the polyline: it can never leave the polyline, never skip a segment and
never teleport. The heading chases the current segment direction with a
bounded turn rate, so there is no instant rotation at corners.

Positions are quantized with the same fixed-point scale as the polyline
coordinates; the per-tick quantization residual is below 1 fixed-point
unit and does not accumulate (position is recomputed from s each tick).
"""

from math import atan2

from . import fp
from .errors import E_CANCELLED, E_PARA, TacError


def _segment_heading_mrad(x0, y0, x1, y1):
    """Quantized segment heading in milli-radians (deterministic)."""
    return int(round(atan2(y1 - y0, x1 - x0) * 1000))


def validate_params(step_fp, turn_mrad):
    if not (0 < step_fp <= fp.MAX_STEP_FP):
        raise TacError(E_PARA, "step length out of range",
                       {"step_fp": step_fp, "max_step_fp": fp.MAX_STEP_FP})
    if not (0 < turn_mrad <= fp.MAX_TURN_MRAD):
        raise TacError(E_PARA, "turn rate out of range",
                       {"turn_mrad": turn_mrad,
                        "max_turn_mrad": fp.MAX_TURN_MRAD})


class AdvanceState:
    """Serializable advance state (all integers)."""

    def __init__(self, s=0, heading_mrad=0, tick=0):
        self.s = s                    # travelled arc length, fixed-point
        self.heading_mrad = heading_mrad
        self.tick = tick

    def to_dict(self):
        return {"s": self.s, "heading_mrad": self.heading_mrad,
                "tick": self.tick}

    @staticmethod
    def from_dict(d):
        return AdvanceState(d["s"], d["heading_mrad"], d["tick"])


def initial_state(waypoints):
    x0, y0 = waypoints[0]
    x1, y1 = waypoints[1]
    return AdvanceState(0, _segment_heading_mrad(x0, y0, x1, y1), 0)


def simulate(waypoints, step_fp, turn_mrad, state=None, max_ticks=1_000_000,
             s_limit=None, cancel_check=None):
    """Advance along `waypoints` (world fixed-point) from `state`.

    Returns (trajectory, state, done_segments). Each trajectory row is
    (tick, x_fp, y_fp, heading_mrad, segment_index, s_fp). The advance
    stops at s_limit (defaults to the polyline end); the step is
    quantized to the limit boundary. cancel_check returning True raises
    E_CANCELLED whose details carry the partial trajectory and state.
    """
    validate_params(step_fp, turn_mrad)
    prefix = fp.path_prefix_lengths(waypoints)
    total = prefix[-1]
    if s_limit is None or s_limit > total:
        s_limit = total
    if state is None:
        state = initial_state(waypoints)
    headings = [_segment_heading_mrad(*w0, *w1)
                for w0, w1 in zip(waypoints, waypoints[1:])]

    trajectory = []
    done_segments = 0
    for i, p in enumerate(prefix[1:], 1):
        if state.s >= p:
            done_segments = i
    s, heading, tick = state.s, state.heading_mrad, state.tick

    while s < s_limit:
        if cancel_check is not None and cancel_check():
            raise TacError(E_CANCELLED, "advance cancelled",
                           {"state": AdvanceState(s, heading, tick).to_dict(),
                            "trajectory": trajectory,
                            "done_segments": done_segments})
        if tick >= max_ticks:
            break
        s = min(s + step_fp, s_limit)
        tick += 1
        x, y, seg = fp.point_at_arclength(waypoints, prefix, s)
        desired = headings[seg]
        diff = fp.wrap_mrad(desired - heading)
        if diff > turn_mrad:
            diff = turn_mrad
        elif diff < -turn_mrad:
            diff = -turn_mrad
        heading = fp.wrap_mrad(heading + diff)
        trajectory.append((tick, x, y, heading, seg, s))
        while (done_segments < len(prefix) - 1
               and prefix[done_segments + 1] <= s):
            done_segments += 1

    return trajectory, AdvanceState(s, heading, tick), done_segments


def on_polyline_check(waypoints, x, y, seg, slack_fp=1):
    """True when (x, y) lies on segment `seg` within `slack_fp` units."""
    x0, y0 = waypoints[seg]
    x1, y1 = waypoints[seg + 1]
    dx, dy = x1 - x0, y1 - y0
    cross = (x - x0) * dy - (y - y0) * dx
    seg_len2 = dx * dx + dy * dy
    if seg_len2 == 0:
        return x == x0 and y == y0
    if cross * cross > slack_fp * slack_fp * seg_len2:
        return False
    dot = (x - x0) * dx + (y - y0) * dy
    if dot < -slack_fp * fp.MAX_STEP_FP:
        return False
    if dot - seg_len2 > slack_fp * fp.MAX_STEP_FP:
        return False
    return True


def quantization_residual_micro(waypoints, prefix, s, x, y):
    """Residual of the quantized position vs the exact rational position.

    Returns micro-units of a fixed-point unit (1_000_000 = 1 fp unit);
    always < 1_000_000 by construction of the integer interpolation.
    """
    ex, ey, seg = fp.point_at_arclength(waypoints, prefix, s)
    return abs(ex - x) * 1_000_000 + abs(ey - y) * 1_000_000
