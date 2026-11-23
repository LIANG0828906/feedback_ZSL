"""Error-propagation report for one search + advance run.

Four tracked statistics, each with a measured value and an upper bound
derived only from observed fixed-point parameters, the neighbourhood
order and the step quantization unit:

  1. cost accumulation error   exact integer accumulation -> bound 0
  2. heuristic deviation       admissible by construction -> bound 0
  3. smoothing cost difference bound = cells*max_cost*DIAG - min_cost
  4. advance quantization residual < 1 fp unit per tick, non-accumulating

Every statistic is compared item-by-item against the analytic ground
truth; any item exceeding its bound is reported with the exact cell or
tick number and surfaces as E_BOUND.
"""

from . import fp
from .errors import E_BOUND, TacError


def _probe_items(truth_probes):
    for key, vals in sorted(truth_probes.items()):
        x, y = (int(v) for v in key.split(","))
        yield (x, y), vals


def build_report(truth, probe_g, heuristic_at, smooth_info, advance_info):
    """Assemble the report dict; `heuristic_at(cell)` returns h in fp."""
    violations = []

    # 1. cost accumulation error vs analytic truth (known cells)
    accum_items = []
    accum_max = 0
    for cell, vals in _probe_items(truth["probes"]):
        expected = vals["from_start"]
        got = probe_g.get(cell)
        err = None if (expected is None or got is None) else abs(got - expected)
        if err:
            accum_max = max(accum_max, err)
            violations.append({"stat": "cost_accumulation",
                               "cell": list(cell), "error_fp": err,
                               "bound_fp": 0})
        accum_items.append({"cell": list(cell), "truth_fp": expected,
                            "measured_fp": got, "error_fp": err})
    cost_accumulation = {"items": accum_items, "max_error_fp": accum_max,
                         "bound_fp": 0,
                         "cumulative_bound": "0 (exact integer accumulation)"}

    # 2. heuristic deviation h(cell) - true remaining cost
    heur_items = []
    heur_max = 0
    for cell, vals in _probe_items(truth["probes"]):
        remaining = vals["to_goal"]
        h = heuristic_at(cell)
        dev = None if remaining is None else max(0, h - remaining)
        if dev:
            heur_max = max(heur_max, dev)
            violations.append({"stat": "heuristic_deviation",
                               "cell": list(cell), "deviation_fp": dev,
                               "bound_fp": 0})
        heur_items.append({"cell": list(cell), "h_fp": h,
                           "true_remaining_fp": remaining,
                           "overestimate_fp": dev})
    heuristic_deviation = {"items": heur_items,
                           "max_overestimate_fp": heur_max,
                           "bound_fp": 0,
                           "cumulative_bound": "0 (admissible by construction)"}

    # 3. smoothing cost difference
    smoothing = {
        "min_cost_fp": smooth_info["min_cost_fp"],
        "smoothed_cost_fp": smooth_info["smoothed_cost_fp"],
        "diff_fp": smooth_info["diff_fp"],
        "bound_fp": smooth_info["bound_fp"],
        "cumulative_bound": ("constant per path: cells_walked * max_cost * "
                             "DIAG - min_cost"),
    }
    if smooth_info["diff_fp"] > smooth_info["bound_fp"]:
        violations.append({"stat": "smoothing_cost",
                           "diff_fp": smooth_info["diff_fp"],
                           "bound_fp": smooth_info["bound_fp"]})

    # 4. advance quantization residual (per tick, non-accumulating)
    adv = advance_info
    if adv["max_residual_micro"] >= 1_000_000:
        violations.append({"stat": "advance_quantization",
                           "tick": adv["max_residual_tick"],
                           "residual_micro": adv["max_residual_micro"],
                           "bound_micro": 1_000_000})
    advance = {
        "ticks": adv["ticks"],
        "max_residual_micro": adv["max_residual_micro"],
        "max_residual_tick": adv["max_residual_tick"],
        "bound_micro_per_tick": 1_000_000,
        "cumulative_bound": ("1 fp unit, constant: position is recomputed "
                             "from arc length every tick, residuals do not "
                             "accumulate"),
        "frame_rate_position_bound_fp": 2,
    }

    return {
        "statistics": {
            "cost_accumulation": cost_accumulation,
            "heuristic_deviation": heuristic_deviation,
            "smoothing_cost": smoothing,
            "advance_quantization": advance,
        },
        "violations": violations,
    }


def enforce_bounds(report):
    """Raise E_BOUND listing every out-of-bound cell/tick, if any."""
    if report["violations"]:
        raise TacError(E_BOUND, "observed error exceeds derived bound",
                       {"violations": report["violations"]})
    return report
