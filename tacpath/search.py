"""Fixed-point minimum-cost search on a weighted grid (A*).

Determinism contract (shared with the reference generator):
neighbour order E, S, W, N then SE, SW, NW, NE (8-neighbourhood), first
four only for 4-neighbourhood; diagonal moves are forbidden when either
orthogonal neighbour of the corner is impassable (no corner cutting);
move cost is the fixed-point cost of the entered cell; priority queue
key is (f, g, x, y) with lazy deletion; a parent is rewritten only on a
strictly smaller g.

All costs accumulate as exact fixed-point integers; the accumulator is
bounded by MAX_COST_FP and raises E_OVERFLOW past it. A heuristic weight
greater than 1 may overestimate the true remaining cost and is rejected
with E_HEUR before any search work happens.
"""

from heapq import heappop, heappush

from . import fp
from .errors import E_HEUR, E_NOPATH, E_OVERFLOW, E_POINT, TacError

NEIGHBORS_8 = [(1, 0), (0, 1), (-1, 0), (0, -1),
               (1, 1), (1, -1), (-1, 1), (-1, -1)]
NEIGHBORS_4 = NEIGHBORS_8[:4]


class SearchResult:
    def __init__(self, path, cost_fp, g_best, expanded):
        self.path = path                # list of (x, y) cells, start..goal
        self.cost_fp = cost_fp          # exact fixed-point minimum cost
        self.g_best = g_best            # settled minimum costs {cell: fp}
        self.expanded = expanded        # number of popped nodes


def _check_point(costs, width, height, point, role):
    x, y = point
    if not (0 <= x < width and 0 <= y < height):
        raise TacError(E_POINT, f"{role} out of bounds",
                       {"role": role, "point": [x, y]})
    if int(costs[y][x]) == 0:
        raise TacError(E_POINT, f"{role} on impassable cell",
                       {"role": role, "point": [x, y]})


def astar(costs, start, goal, neighborhood=8, weight_num=1, weight_den=1):
    """Run deterministic fixed-point A*.

    `costs` is a 2-D indexable (list of lists or NumPy array) of integer
    cell costs, 0 = impassable. With goal=None the search runs as
    Dijkstra over the whole reachable map and only g_best is meaningful.
    """
    if weight_den <= 0 or weight_num < 0:
        raise TacError(E_HEUR, "invalid heuristic weight",
                       {"weight_num": weight_num, "weight_den": weight_den})
    if weight_num > weight_den:
        raise TacError(E_HEUR,
                       "heuristic weight > 1 may overestimate true cost",
                       {"weight_num": weight_num, "weight_den": weight_den})
    if neighborhood == 8:
        neighbors = NEIGHBORS_8
    elif neighborhood == 4:
        neighbors = NEIGHBORS_4
    else:
        raise TacError(E_HEUR, "neighborhood must be 4 or 8",
                       {"neighborhood": neighborhood})

    height = len(costs)
    width = len(costs[0])
    _check_point(costs, width, height, start, "start")
    if goal is not None:
        _check_point(costs, width, height, goal, "goal")

    min_cell_cost = None
    for row in costs:
        for c in row:
            c = int(c)
            if c and (min_cell_cost is None or c < min_cell_cost):
                min_cell_cost = c

    def h(x, y):
        if goal is None:
            return 0
        return fp.heuristic_fp(goal[0] - x, goal[1] - y,
                               min_cell_cost, weight_num, weight_den)

    g_best = {start: 0}
    parent = {start: None}
    heap = [(h(*start), 0, start[0], start[1])]
    expanded = 0
    found = goal is None

    while heap:
        f, g, x, y = heappop(heap)
        if g > g_best.get((x, y), fp.MAX_COST_FP):
            continue  # stale lazy-deletion entry
        expanded += 1
        if goal is not None and (x, y) == goal:
            found = True
            break
        for dx, dy in neighbors:
            nx, ny = x + dx, y + dy
            if not (0 <= nx < width and 0 <= ny < height):
                continue
            cell_cost = int(costs[ny][nx])
            if cell_cost == 0:
                continue
            diagonal = dx != 0 and dy != 0
            if diagonal and (int(costs[y][nx]) == 0
                             or int(costs[ny][x]) == 0):
                continue  # no corner cutting
            ng = g + fp.move_cost_fp(cell_cost, diagonal)
            if ng > fp.MAX_COST_FP:
                raise TacError(E_OVERFLOW, "cost accumulator overflow",
                               {"cell": [nx, ny], "g_fp": ng,
                                "bound_fp": fp.MAX_COST_FP})
            if ng < g_best.get((nx, ny), fp.MAX_COST_FP):
                g_best[(nx, ny)] = ng
                parent[(nx, ny)] = (x, y)
                heappush(heap, (ng + h(nx, ny), ng, nx, ny))

    if not found:
        raise TacError(E_NOPATH, "no path between start and goal",
                       {"start": list(start), "goal": list(goal)})

    if goal is None:
        return SearchResult(None, 0, g_best, expanded)

    path = []
    node = goal
    while node is not None:
        path.append(node)
        node = parent[node]
    path.reverse()
    return SearchResult(path, g_best[goal], g_best, expanded)


def dijkstra_costs(costs, start, neighborhood=8):
    """Exact fixed-point minimum costs from start to every reachable cell."""
    return astar(costs, start, None, neighborhood=neighborhood).g_best
