"""Orchestration: search -> smooth -> advance with segmented checkpoints.

Three execution paths over the same (map, pair, params):
  realtime   single uninterrupted run (still checkpoints per segment)
  resume     continues from the persisted snapshot of an interrupted run
  recompute  ignores and discards any snapshot, full recompute
All three must produce identical paths, corners and trajectories;
compare_outputs raises E_DIFF at the first differing cell or tick.
"""

from . import advance, fp, report as report_mod, search, smooth
from .checkpoint import ALGO_VERSION, CheckpointStore, param_version, \
    segment_hash
from .errors import E_CANCELLED, E_DIFF, TacError

DEFAULT_PARAMS = {
    "fp_scale": fp.SCALE,
    "neighborhood": 8,
    "weight_num": 1,
    "weight_den": 1,
    "tolerance_fp": 0,
    "step_fp": 200,
    "turn_mrad": 300,
}

MAX_SEGMENT_RETRIES = 1


class RunOutput:
    def __init__(self, mode, path, cost_fp, corners_grid, corners_world,
                 smoothed_cost_fp, trajectory, report):
        self.mode = mode
        self.path = path
        self.cost_fp = cost_fp
        self.corners_grid = corners_grid
        self.corners_world = corners_world
        self.smoothed_cost_fp = smoothed_cost_fp
        self.trajectory = trajectory
        self.report = report

    def to_dict(self):
        return {"mode": self.mode, "path": [list(p) for p in self.path],
                "cost_fp": self.cost_fp,
                "corners_grid": [list(c) for c in self.corners_grid],
                "corners_world": [list(c) for c in self.corners_world],
                "smoothed_cost_fp": self.smoothed_cost_fp,
                "trajectory": [list(t) for t in self.trajectory],
                "report": self.report}


def _min_cell_cost(costs):
    best = None
    for row in costs:
        for c in row:
            c = int(c)
            if c and (best is None or c < best):
                best = c
    return best


def _fresh_snapshot(map_hash, p_version):
    return {"map_hash": map_hash, "param_version": p_version,
            "algo_version": ALGO_VERSION,
            "search": None, "smooth": None,
            "advance": {"done_segments": 0, "ticks": [], "state": None}}


def run_pipeline(map_data, pair_index, params, store, mode,
                 cancel_after_segment=None, fault_hook=None):
    """Run the full pipeline in one of: realtime / resume / recompute."""
    assert mode in ("realtime", "resume", "recompute")
    start, goal = map_data.pairs[pair_index]
    neighborhood = params["neighborhood"]
    p_version = param_version(params)

    if mode == "recompute":
        store.discard()
        snapshot = None
    else:
        snapshot = store.load(map_data.content_hash, p_version)
    if snapshot is None:
        snapshot = _fresh_snapshot(map_data.content_hash, p_version)

    # search phase (checkpointed as one block)
    if snapshot["search"] is not None:
        path = [tuple(p) for p in snapshot["search"]["path"]]
        cost_fp = snapshot["search"]["cost_fp"]
    else:
        res = search.astar(map_data.costs, start, goal,
                           neighborhood=neighborhood,
                           weight_num=params["weight_num"],
                           weight_den=params["weight_den"])
        path, cost_fp = res.path, res.cost_fp
        snapshot["search"] = {"path": [list(p) for p in path],
                              "cost_fp": cost_fp,
                              "hash": segment_hash(
                                  {"path": path, "cost_fp": cost_fp})}
        store.save(snapshot)

    # smoothing phase (checkpointed as one block)
    if snapshot["smooth"] is not None:
        corners = [tuple(c) for c in snapshot["smooth"]["corners"]]
        corners_w = [tuple(c) for c in snapshot["smooth"]["corners_world"]]
        smoothed_cost = snapshot["smooth"]["cost_fp"]
    else:
        corners = smooth.greedy_smooth(map_data.costs, path,
                                       params["tolerance_fp"])
        corners_w = smooth.corners_world(corners)
        smoothed_cost, cells_walked = smooth.polyline_cost_fp(
            map_data.costs, corners)
        bound = smooth.smooth_cost_bound_fp(cells_walked,
                                            map_data.max_cell_cost, cost_fp)
        smooth.validate_smoothing(smoothed_cost, cost_fp, bound)
        snapshot["smooth"] = {
            "corners": [list(c) for c in corners],
            "corners_world": [list(c) for c in corners_w],
            "cost_fp": smoothed_cost,
            "hash": segment_hash({"corners": corners,
                                  "cost_fp": smoothed_cost})}
        store.save(snapshot)

    # advance phase, segment by segment, checkpoint after each
    waypoints = corners_w
    prefix = fp.path_prefix_lengths(waypoints)
    n_segments = len(waypoints) - 1
    adv_snap = snapshot["advance"]
    ticks = [tuple(t) for t in adv_snap["ticks"]]
    if adv_snap["state"] is not None:
        state = advance.AdvanceState.from_dict(adv_snap["state"])
    else:
        state = advance.initial_state(waypoints)

    for seg in range(adv_snap["done_segments"], n_segments):
        target_s = prefix[seg + 1]
        attempts = 0
        while True:
            try:
                if fault_hook is not None:
                    fault_hook(seg)
                seg_ticks, state, _ = advance.simulate(
                    waypoints, params["step_fp"], params["turn_mrad"],
                    state=state, s_limit=target_s)
                for t in seg_ticks:
                    if not advance.on_polyline_check(waypoints, t[1], t[2],
                                                     t[4]):
                        raise TacError("E_SEGMENT",
                                       "tick left the polyline",
                                       {"tick": t[0], "segment": seg})
                break
            except TacError:
                attempts += 1
                if attempts > MAX_SEGMENT_RETRIES:
                    raise
                # segment failed: retry it; completed segments untouched
        ticks.extend(seg_ticks)
        adv_snap["ticks"] = [list(t) for t in ticks]
        adv_snap["state"] = state.to_dict()
        adv_snap["done_segments"] = seg + 1
        adv_snap["hash"] = segment_hash(adv_snap["ticks"])
        store.save(snapshot)
        if (cancel_after_segment is not None
                and seg + 1 == cancel_after_segment):
            raise TacError(E_CANCELLED,
                           "cancelled after segment checkpoint",
                           {"done_segments": seg + 1})

    # error propagation report vs analytic truth
    truth = map_data.truth[str(pair_index)]
    probe_g = search.dijkstra_costs(map_data.costs, start, neighborhood)
    min_cell = _min_cell_cost(map_data.costs)

    def heuristic_at(cell):
        return fp.heuristic_fp(goal[0] - cell[0], goal[1] - cell[1],
                               min_cell, params["weight_num"],
                               params["weight_den"])

    max_residual = 0
    max_residual_tick = None
    for t in ticks:
        r = advance.quantization_residual_micro(waypoints, prefix,
                                                t[5], t[1], t[2])
        if r > max_residual:
            max_residual, max_residual_tick = r, t[0]

    smooth_info = {
        "min_cost_fp": cost_fp,
        "smoothed_cost_fp": smoothed_cost,
        "diff_fp": smoothed_cost - cost_fp,
        "bound_fp": smooth.smooth_cost_bound_fp(
            smooth.polyline_cost_fp(map_data.costs, corners)[1],
            map_data.max_cell_cost, cost_fp),
    }
    advance_info = {"ticks": len(ticks),
                    "max_residual_micro": max_residual,
                    "max_residual_tick": max_residual_tick}
    rep = report_mod.build_report(truth, probe_g, heuristic_at,
                                  smooth_info, advance_info)

    # zero-error truth checks: goal cost, standard path length, corners
    raw_corners = [list(c) for c in smooth.extract_raw_corners(path)]
    goal_err = abs(cost_fp - truth["cost_fp"])
    rep["truth_check"] = {
        "goal_cost": {"truth_fp": truth["cost_fp"],
                      "measured_fp": cost_fp, "error_fp": goal_err},
        "path_len": {"truth": truth["path_len"], "measured": len(path)},
        "corners": {"truth": truth["corners"], "measured": raw_corners,
                    "match": truth["corners"] == raw_corners},
    }
    if goal_err:
        rep["violations"].append({"stat": "goal_cost",
                                  "error_fp": goal_err, "bound_fp": 0})
    if (truth["path_len"] != len(path)
            or not rep["truth_check"]["corners"]["match"]):
        rep["violations"].append({"stat": "path_structure",
                                  "truth_path_len": truth["path_len"],
                                  "measured_path_len": len(path)})
    report_mod.enforce_bounds(rep)

    return RunOutput(mode, path, cost_fp, corners, corners_w,
                     smoothed_cost, ticks, rep)


def compare_outputs(a, b):
    """Raise E_DIFF at the first divergence between two run outputs."""
    if a.path != b.path:
        for i, (pa, pb) in enumerate(zip(a.path, b.path)):
            if pa != pb:
                raise TacError(E_DIFF, "path cells diverge",
                               {"kind": "path", "index": i,
                                "cell_a": list(pa), "cell_b": list(pb)})
        raise TacError(E_DIFF, "path lengths diverge",
                       {"kind": "path", "len_a": len(a.path),
                        "len_b": len(b.path)})
    if a.corners_grid != b.corners_grid:
        raise TacError(E_DIFF, "corner sequences diverge",
                       {"kind": "corners",
                        "corners_a": [list(c) for c in a.corners_grid],
                        "corners_b": [list(c) for c in b.corners_grid]})
    ta, tb = a.trajectory, b.trajectory
    for i in range(min(len(ta), len(tb))):
        if ta[i] != tb[i]:
            raise TacError(E_DIFF, "trajectories diverge",
                           {"kind": "trajectory", "tick": ta[i][0],
                            "segment": ta[i][4], "tick_a": list(ta[i]),
                            "tick_b": list(tb[i])})
    if len(ta) != len(tb):
        raise TacError(E_DIFF, "trajectory lengths diverge",
                       {"kind": "trajectory", "len_a": len(ta),
                        "len_b": len(tb)})
    return True


def replay_tick(outputs, tick):
    """Return each run's trajectory record at `tick` for inspection."""
    result = {}
    for mode, out in outputs.items():
        rec = next((t for t in out.trajectory if t[0] == tick), None)
        result[mode] = None if rec is None else list(rec)
    return result


def replay_segment(map_data, pair_index, params, seg_index):
    """Deterministically re-execute one advance segment for review."""
    store = CheckpointStore("/dev/null/nonexistent")
    store.save = lambda snapshot: None
    out = run_pipeline(map_data, pair_index, params, store, "recompute")
    return [list(t) for t in out.trajectory if t[4] == seg_index]


def frame_rate_deviation(waypoints, step_fp, turn_mrad):
    """Max world-coordinate deviation between two frame-rate samplings.

    Compares positions at identical arc lengths; bound is 2 fp units
    (independent rounding of each run's integer interpolation).
    """
    traj_a, _, _ = advance.simulate(waypoints, step_fp, turn_mrad)
    step_b = max(1, step_fp // 2)
    traj_b, _, _ = advance.simulate(waypoints, step_b, turn_mrad)
    pos_b = {t[5]: (t[1], t[2]) for t in traj_b}
    max_dev = 0
    for t in traj_a:
        if t[5] in pos_b:
            bx, by = pos_b[t[5]]
            max_dev = max(max_dev, abs(t[1] - bx), abs(t[2] - by))
    return {"max_deviation_fp": max_dev, "bound_fp": 2}
