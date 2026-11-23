"""Line-of-sight path smoothing and corner extraction (fixed-point).

Smoothing replaces sub-paths by straight segments when
  (a) the supercover of the segment only touches passable cells, and
  (b) every skipped original path cell lies within `tolerance_fp`
      fixed-point cell units of the segment.
Tightening the tolerance therefore never removes corners: corner count
is monotone non-decreasing as tolerance shrinks.

All geometry is exact integer arithmetic; no floats are involved.
"""

from . import fp
from .errors import E_SMOOTH, TacError


def supercover_line(x0, y0, x1, y1):
    """All grid cells touched by the segment between two cell centers.

    Exact DDA in integer arithmetic; on grid-corner crossings both
    orthogonal cells are included, so the result is a conservative
    supercover that can never slip through a blocked diagonal corner.
    """
    cells = [(x0, y0)]
    dx, dy = x1 - x0, y1 - y0
    if dx == 0 and dy == 0:
        return cells
    sx = 1 if dx > 0 else -1
    sy = 1 if dy > 0 else -1
    adx, ady = abs(dx), abs(dy)
    x, y = x0, y0
    tx_num, tx_den = (1, 2 * adx) if adx else (0, 0)
    ty_num, ty_den = (1, 2 * ady) if ady else (0, 0)
    while (x, y) != (x1, y1):
        go_x = tx_den and (not ty_den or tx_num * ty_den < ty_num * tx_den)
        go_y = ty_den and (not tx_den or ty_num * tx_den < tx_num * ty_den)
        if go_x:
            x += sx
            tx_num += 2
            cells.append((x, y))
        elif go_y:
            y += sy
            ty_num += 2
            cells.append((x, y))
        else:  # exact grid-corner crossing: cover both orthogonal cells
            cells.append((x + sx, y))
            cells.append((x, y + sy))
            x += sx
            y += sy
            tx_num += 2
            ty_num += 2
            cells.append((x, y))
    return cells


def los_passable(costs, a, b):
    """True when every supercover cell of segment a->b is passable."""
    for x, y in supercover_line(a[0], a[1], b[0], b[1]):
        if int(costs[y][x]) == 0:
            return False
    return True


def _deviation_ok(path, i, j, tolerance_fp):
    """All original cells path[i..j] within tolerance of segment i->j."""
    ax, ay = path[i]
    bx, by = path[j]
    dx, dy = bx - ax, by - ay
    seg_len2 = dx * dx + dy * dy
    if seg_len2 == 0:
        return True
    tol2 = tolerance_fp * tolerance_fp
    scale2 = fp.SCALE * fp.SCALE
    for k in range(i, j + 1):
        px, py = path[k]
        cross = (px - ax) * dy - (py - ay) * dx
        if cross * cross * scale2 > tol2 * seg_len2:
            return False
    return True


def greedy_smooth(costs, path, tolerance_fp):
    """Greedy farthest-visible smoothing; returns corner cell list."""
    if tolerance_fp < 0:
        raise TacError(E_SMOOTH, "negative smoothing tolerance",
                       {"tolerance_fp": tolerance_fp})
    n = len(path)
    corners = [path[0]]
    i = 0
    while i < n - 1:
        j = n - 1
        while j > i + 1:
            if (los_passable(costs, path[i], path[j])
                    and _deviation_ok(path, i, j, tolerance_fp)):
                break
            j -= 1
        corners.append(path[j])
        i = j
    return corners


def extract_raw_corners(path):
    """Direction-change points of a grid path (endpoints included)."""
    if len(path) <= 2:
        return list(path)
    corners = [path[0]]
    for k in range(1, len(path) - 1):
        d0 = (path[k][0] - path[k - 1][0], path[k][1] - path[k - 1][1])
        d1 = (path[k + 1][0] - path[k][0], path[k + 1][1] - path[k][1])
        if d0 != d1:
            corners.append(path[k])
    corners.append(path[-1])
    return corners


def corners_world(corners):
    """Dual readout: fixed-point world coordinates of corner cells."""
    return [fp.cell_center_world(x, y) for x, y in corners]


def bresenham_line(x0, y0, x1, y1):
    """8-connected Bresenham walk between two cell centers.

    Used for cost accounting: it is the cheapest grid walk of the
    segment.  Every cell it touches is covered by the supercover, so a
    segment that passed the LOS check is fully walkable this way.
    """
    cells = [(x0, y0)]
    dx, dy = abs(x1 - x0), abs(y1 - y0)
    sx = 1 if x1 > x0 else -1
    sy = 1 if y1 > y0 else -1
    x, y = x0, y0
    err = dx - dy
    while (x, y) != (x1, y1):
        e2 = 2 * err
        if e2 > -dy:
            err -= dy
            x += sx
        if e2 < dx:
            err += dx
            y += sy
        cells.append((x, y))
    return cells


def polyline_cost_fp(costs, corners):
    """Fixed-point movement cost of walking the smoothed polyline.

    Walks each segment's Bresenham cells and accumulates the same move
    cost model as the search, so the result is directly comparable to
    the minimum path cost. Also returns the walked cell count.
    """
    total = 0
    cells_walked = 0
    for a, b in zip(corners, corners[1:]):
        line = bresenham_line(a[0], a[1], b[0], b[1])
        for (x0, y0), (x1, y1) in zip(line, line[1:]):
            c = int(costs[y1][x1])
            if c == 0:
                raise TacError(E_SMOOTH, "smoothed path crosses impassable "
                               "cell", {"cell": [x1, y1]})
            total += fp.move_cost_fp(c, x0 != x1 and y0 != y1)
        cells_walked += len(line) - 1
    return total, cells_walked


def smooth_cost_bound_fp(cells_walked, max_cell_cost, min_cost_fp):
    """A-priori upper bound for (smoothed_cost - min_cost).

    Derived only from observed fixed-point parameters: every walked move
    costs at most max_cell_cost * DIAG, so the smoothed cost is at most
    cells_walked * max_cell_cost * DIAG.
    """
    return cells_walked * max_cell_cost * fp.DIAG - min_cost_fp


def validate_smoothing(smoothed_cost_fp, min_cost_fp, bound_fp):
    """Enforce smoothing invariants; raises E_SMOOTH on violation."""
    if smoothed_cost_fp < min_cost_fp:
        raise TacError(E_SMOOTH,
                       "smoothed cost below the minimum path cost",
                       {"smoothed_cost_fp": smoothed_cost_fp,
                        "min_cost_fp": min_cost_fp})
    diff = smoothed_cost_fp - min_cost_fp
    if diff > bound_fp:
        raise TacError(E_SMOOTH,
                       "smoothed cost difference exceeds derived bound",
                       {"diff_fp": diff, "bound_fp": bound_fp})
    return diff
