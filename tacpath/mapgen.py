"""Deterministic tactical map + analytic ground-truth generator.

Uses only the standard library (random.Random seeded per map) so the
generated samples are reproducible across runs and NumPy versions.

Ground truth is produced by an independent reference implementation of
the documented deterministic search contract (see tacpath.search):
reference_astar shares the same tie-breaking rules but is written
independently; reference_dijkstra gives exact minimum costs from one
source to all cells.

Truth entries per (start, goal) pair:
  cost_fp   exact fixed-point minimum cost start -> goal
  path_len  number of cells of the reference minimum-cost path
  corners   direction-change cells of the reference path (grid coords)
  probes    per probe cell: from_start / to_goal exact minimum costs
"""

import json
import random
from heapq import heappop, heappush

from . import fp

TERRAIN_PLAIN = 0
TERRAIN_FOREST = 1
TERRAIN_HILL = 2
TERRAIN_WATER = 3  # impassable marker; cost must be 0

TERRAIN_COST_RANGES = {
    TERRAIN_PLAIN: (1, 2),
    TERRAIN_FOREST: (3, 5),
    TERRAIN_HILL: (6, 9),
}

GENERATOR_VERSION = "tacpath-mapgen/1.0.0"

_NEIGHBORS_8 = [(1, 0), (0, 1), (-1, 0), (0, -1),
                (1, 1), (1, -1), (-1, 1), (-1, -1)]


def _gen_grid(rng, width, height):
    costs = [[0] * width for _ in range(height)]
    terrain = [[TERRAIN_PLAIN] * width for _ in range(height)]
    for y in range(height):
        for x in range(width):
            r = rng.random()
            if r < 0.10:
                terrain[y][x] = TERRAIN_WATER
                costs[y][x] = 0
            elif r < 0.45:
                terrain[y][x] = TERRAIN_PLAIN
                costs[y][x] = rng.randint(*TERRAIN_COST_RANGES[TERRAIN_PLAIN])
            elif r < 0.80:
                terrain[y][x] = TERRAIN_FOREST
                costs[y][x] = rng.randint(*TERRAIN_COST_RANGES[TERRAIN_FOREST])
            else:
                terrain[y][x] = TERRAIN_HILL
                costs[y][x] = rng.randint(*TERRAIN_COST_RANGES[TERRAIN_HILL])
    return costs, terrain


def _largest_component(costs, width, height):
    seen = [[False] * width for _ in range(height)]
    best = []
    for y0 in range(height):
        for x0 in range(width):
            if seen[y0][x0] or costs[y0][x0] == 0:
                continue
            comp = []
            stack = [(x0, y0)]
            seen[y0][x0] = True
            while stack:
                x, y = stack.pop()
                comp.append((x, y))
                for dx, dy in _NEIGHBORS_8[:4]:
                    nx, ny = x + dx, y + dy
                    if (0 <= nx < width and 0 <= ny < height
                            and not seen[ny][nx] and costs[ny][nx] != 0):
                        seen[ny][nx] = True
                        stack.append((nx, ny))
            if len(comp) > len(best):
                best = comp
    return best


def reference_astar(costs, start, goal, neighborhood=8):
    """Independent reference implementation of the search contract."""
    height, width = len(costs), len(costs[0])
    neighbors = _NEIGHBORS_8 if neighborhood == 8 else _NEIGHBORS_8[:4]
    min_cost = min(c for row in costs for c in row if c)

    def h(x, y):
        return fp.heuristic_fp(goal[0] - x, goal[1] - y, min_cost, 1, 1)

    g_best = {start: 0}
    parent = {start: None}
    heap = [(h(*start), 0, start[0], start[1])]
    while heap:
        f, g, x, y = heappop(heap)
        if g > g_best.get((x, y), fp.MAX_COST_FP):
            continue
        if (x, y) == goal:
            break
        for dx, dy in neighbors:
            nx, ny = x + dx, y + dy
            if not (0 <= nx < width and 0 <= ny < height):
                continue
            c = costs[ny][nx]
            if c == 0:
                continue
            diag = dx != 0 and dy != 0
            if diag and (costs[y][nx] == 0 or costs[ny][x] == 0):
                continue
            ng = g + fp.move_cost_fp(c, diag)
            if ng < g_best.get((nx, ny), fp.MAX_COST_FP):
                g_best[(nx, ny)] = ng
                parent[(nx, ny)] = (x, y)
                heappush(heap, (ng + h(nx, ny), ng, nx, ny))
    if goal not in g_best or (goal != start and goal not in parent):
        return None, None
    path = []
    node = goal
    while node is not None:
        path.append(node)
        node = parent[node]
    path.reverse()
    return path, g_best[goal]


def reference_dijkstra(costs, start, neighborhood=8):
    """Exact minimum fixed-point costs from start to all reachable cells."""
    height, width = len(costs), len(costs[0])
    neighbors = _NEIGHBORS_8 if neighborhood == 8 else _NEIGHBORS_8[:4]
    dist = {start: 0}
    heap = [(0, start[0], start[1])]
    while heap:
        g, x, y = heappop(heap)
        if g > dist.get((x, y), fp.MAX_COST_FP):
            continue
        for dx, dy in neighbors:
            nx, ny = x + dx, y + dy
            if not (0 <= nx < width and 0 <= ny < height):
                continue
            c = costs[ny][nx]
            if c == 0:
                continue
            diag = dx != 0 and dy != 0
            if diag and (costs[y][nx] == 0 or costs[ny][x] == 0):
                continue
            ng = g + fp.move_cost_fp(c, diag)
            if ng < dist.get((nx, ny), fp.MAX_COST_FP):
                dist[(nx, ny)] = ng
                heappush(heap, (ng, nx, ny))
    return dist


def _raw_corners(path):
    if len(path) <= 2:
        return [list(p) for p in path]
    corners = [path[0]]
    for k in range(1, len(path) - 1):
        d0 = (path[k][0] - path[k - 1][0], path[k][1] - path[k - 1][1])
        d1 = (path[k + 1][0] - path[k][0], path[k + 1][1] - path[k][1])
        if d0 != d1:
            corners.append(path[k])
    corners.append(path[-1])
    return [list(p) for p in corners]


def generate_map(map_id, seed, width, height, n_pairs=2, n_probes=4,
                 neighborhood=8):
    """Generate one deterministic map with analytic ground truth."""
    rng = random.Random(seed)
    costs, terrain = _gen_grid(rng, width, height)
    component = _largest_component(costs, width, height)
    rng.shuffle(component)

    pairs = []
    used = set()
    for _ in range(n_pairs):
        for attempt in range(200):
            a = component[rng.randrange(len(component))]
            b = component[rng.randrange(len(component))]
            if a == b or (a, b) in used:
                continue
            if abs(a[0] - b[0]) + abs(a[1] - b[1]) < max(width, height) // 2:
                continue
            path, cost = reference_astar(costs, a, b, neighborhood)
            if path is not None:
                used.add((a, b))
                pairs.append((a, b))
                break

    probes = []
    for cell in component:
        if len(probes) >= n_probes:
            break
        if all(cell != a and cell != b for a, b in pairs):
            probes.append(cell)

    truth = {}
    for idx, (a, b) in enumerate(pairs):
        path, cost = reference_astar(costs, a, b, neighborhood)
        from_start = reference_dijkstra(costs, a, neighborhood)
        probe_truth = {}
        for px, py in probes:
            to_goal = reference_dijkstra(costs, (px, py), neighborhood)
            probe_truth[f"{px},{py}"] = {
                "from_start": from_start.get((px, py)),
                "to_goal": to_goal.get(b),
            }
        truth[str(idx)] = {
            "cost_fp": cost,
            "path_len": len(path),
            "corners": _raw_corners(path),
            "probes": probe_truth,
        }

    return {
        "id": map_id,
        "generator": GENERATOR_VERSION,
        "seed": seed,
        "neighborhood": neighborhood,
        "width": width,
        "height": height,
        "costs": costs,
        "terrain": terrain,
        "pairs": [{"start": list(a), "goal": list(b)} for a, b in pairs],
        "probes": [list(p) for p in probes],
        "truth": truth,
    }


MAP_SPECS = [
    ("map_0", 20261001, 16, 12),
    ("map_1", 20261002, 31, 24),
    ("map_2", 20261003, 64, 48),
    ("map_3", 20261004, 48, 40),
]


def generate_all(out_dir):
    """Generate the full deterministic sample set + manifest."""
    import os
    os.makedirs(out_dir, exist_ok=True)
    entries = []
    for map_id, seed, w, h in MAP_SPECS:
        m = generate_map(map_id, seed, w, h)
        fname = f"{map_id}.json"
        with open(os.path.join(out_dir, fname), "w") as fh:
            json.dump(m, fh)
        entries.append({"file": fname, "id": map_id, "seed": seed,
                        "width": w, "height": h,
                        "pairs": len(m["pairs"]), "probes": len(m["probes"])})
    manifest = {"version": 1, "generator": GENERATOR_VERSION,
                "maps": entries}
    with open(os.path.join(out_dir, "manifest.json"), "w") as fh:
        json.dump(manifest, fh, indent=2)
    return manifest
