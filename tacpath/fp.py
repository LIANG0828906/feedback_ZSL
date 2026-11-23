"""Fixed-point arithmetic shared by search, smoothing and advance.

All fractional quantities are integers scaled by SCALE (=1000).
Grid costs are integers; movement costs are expressed in fixed-point so
that diagonal moves (x sqrt(2) ~ 1.414) accumulate *exactly* as integers
and never introduce rounding drift.

World coordinates are fixed-point integers at the same SCALE; a cell
center (x, y) maps to world (x*SCALE + SCALE/2, y*SCALE + SCALE/2).
"""

from math import isqrt

SCALE = 1000
ORTHO = 1000          # fixed-point multiplier for orthogonal moves (1.0)
DIAG = 1414           # fixed-point multiplier for diagonal moves (~1.414)

# Explicit overflow boundary for every fixed-point cost accumulator.
MAX_COST_FP = 1 << 62

# Legal integer range for a single cell's traversal cost (0 = impassable).
MAX_CELL_COST = 32767

# Angles are milli-radians stored as integers.
PI_MRAD = 3142
TWO_PI_MRAD = 6283

# Advance-parameter bounds (checked with E_PARA).
MAX_STEP_FP = 100 * SCALE
MAX_TURN_MRAD = PI_MRAD


def move_cost_fp(cell_cost, diagonal):
    """Fixed-point cost of entering a cell of the given integer cost."""
    return cell_cost * (DIAG if diagonal else ORTHO)


def octile_fp(dx, dy):
    """Octile distance between two cells, in fixed-point cell units."""
    dx, dy = abs(dx), abs(dy)
    lo, hi = min(dx, dy), max(dx, dy)
    return lo * DIAG + (hi - lo) * ORTHO


def heuristic_fp(dx, dy, min_cell_cost, weight_num, weight_den):
    """Admissible fixed-point heuristic.

    octile * cheapest cell cost is a lower bound on any remaining path;
    weight_num/weight_den <= 1 keeps it non-overestimating.  Integer
    floor division only ever rounds the estimate *down*.
    """
    return octile_fp(dx, dy) * min_cell_cost * weight_num // weight_den


def cell_center_world(x, y):
    return (x * SCALE + SCALE // 2, y * SCALE + SCALE // 2)


def seg_len_fp(dx, dy):
    """Floor of the fixed-point segment length (exact integer sqrt)."""
    return isqrt(dx * dx + dy * dy)


def path_prefix_lengths(waypoints):
    """Prefix sums of fixed-point segment lengths along a world polyline."""
    prefix = [0]
    for (x0, y0), (x1, y1) in zip(waypoints, waypoints[1:]):
        prefix.append(prefix[-1] + seg_len_fp(x1 - x0, y1 - y0))
    return prefix


def point_at_arclength(waypoints, prefix, s):
    """Position (x_fp, y_fp, segment_index) at fixed-point arc length s.

    Pure integer interpolation: the result deviates from the exact
    rational position by less than 1 fixed-point unit per axis.
    """
    if s <= 0:
        return waypoints[0][0], waypoints[0][1], 0
    total = prefix[-1]
    if s >= total:
        return waypoints[-1][0], waypoints[-1][1], len(waypoints) - 2
    seg = 0
    while prefix[seg + 1] < s:
        seg += 1
    x0, y0 = waypoints[seg]
    x1, y1 = waypoints[seg + 1]
    seg_len = prefix[seg + 1] - prefix[seg]
    rem = s - prefix[seg]
    if seg_len == 0:
        return x0, y0, seg
    x = x0 + (x1 - x0) * rem // seg_len
    y = y0 + (y1 - y0) * rem // seg_len
    return x, y, seg


def wrap_mrad(angle):
    """Wrap a milli-radian angle into (-PI_MRAD, PI_MRAD]."""
    while angle > PI_MRAD:
        angle -= TWO_PI_MRAD
    while angle <= -PI_MRAD:
        angle += TWO_PI_MRAD
    return angle
