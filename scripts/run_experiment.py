#!/usr/bin/env python3
"""Reproducible experiment runner (no network, no external services).

Usage:
  python scripts/run_experiment.py gen          # generate maps + truth
  python scripts/run_experiment.py run          # run all experiments
  python scripts/run_experiment.py all          # gen + run

For every (map, pair) the runner executes the three required paths:
  realtime   uninterrupted run with per-segment checkpoints
  resume     run interrupted after segment 1, then resumed from checkpoint
  recompute  full recompute with the snapshot discarded
and verifies cell-by-cell / tick-by-tick equality (E_DIFF on mismatch).
Reports land in results/, checkpoints in results/checkpoints/.
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tacpath import mapgen, mapio, pipeline
from tacpath.checkpoint import CheckpointStore
from tacpath.errors import E_CANCELLED, TacError

MAPS_DIR = "maps"
RESULTS_DIR = "results"


def cmd_gen():
    manifest = mapgen.generate_all(MAPS_DIR)
    print(f"generated {len(manifest['maps'])} maps into {MAPS_DIR}/")
    for e in manifest["maps"]:
        print(f"  {e['id']}: {e['width']}x{e['height']} "
              f"pairs={e['pairs']} probes={e['probes']}")


def _run_three_paths(m, pair_index, ckpt_dir):
    params = dict(pipeline.DEFAULT_PARAMS)
    os.makedirs(ckpt_dir, exist_ok=True)
    for name in ("realtime.json", "resume.json", "recompute.json"):
        stale = os.path.join(ckpt_dir, name)
        if os.path.exists(stale):
            os.remove(stale)

    realtime_store = CheckpointStore(os.path.join(ckpt_dir, "realtime.json"))
    out_realtime = pipeline.run_pipeline(m, pair_index, params,
                                         realtime_store, "realtime")

    resume_store = CheckpointStore(os.path.join(ckpt_dir, "resume.json"))
    try:
        pipeline.run_pipeline(m, pair_index, params, resume_store,
                              "realtime", cancel_after_segment=1)
        raise AssertionError("expected E_CANCELLED")
    except TacError as exc:
        assert exc.code == E_CANCELLED, exc
    out_resume = pipeline.run_pipeline(m, pair_index, params,
                                       resume_store, "resume")

    recompute_store = CheckpointStore(
        os.path.join(ckpt_dir, "recompute.json"))
    pipeline.run_pipeline(m, pair_index, params, recompute_store,
                          "realtime")  # create a stale snapshot
    out_recompute = pipeline.run_pipeline(m, pair_index, params,
                                          recompute_store, "recompute")

    pipeline.compare_outputs(out_realtime, out_resume)
    pipeline.compare_outputs(out_realtime, out_recompute)
    pipeline.compare_outputs(out_resume, out_recompute)
    return out_realtime


def cmd_run():
    repo = mapio.MapRepository()
    repo.load_all(os.path.join(MAPS_DIR, "manifest.json"))
    os.makedirs(RESULTS_DIR, exist_ok=True)
    summary = []
    for map_id, m in sorted(repo.maps.items()):
        for pair_index in range(len(m.pairs)):
            tag = f"{map_id}_pair{pair_index}"
            ckpt_dir = os.path.join(RESULTS_DIR, "checkpoints", tag)
            out = _run_three_paths(m, pair_index, ckpt_dir)

            fr = pipeline.frame_rate_deviation(
                [tuple(c) for c in out.corners_world],
                pipeline.DEFAULT_PARAMS["step_fp"],
                pipeline.DEFAULT_PARAMS["turn_mrad"])

            record = {
                "map": map_id,
                "pair": pair_index,
                "start": list(m.pairs[pair_index][0]),
                "goal": list(m.pairs[pair_index][1]),
                "cost_fp": out.cost_fp,
                "corners_grid": [list(c) for c in out.corners_grid],
                "corners_world": [list(c) for c in out.corners_world],
                "ticks": len(out.trajectory),
                "three_path_consistency": "OK",
                "frame_rate_check": fr,
                "report": out.report,
            }
            rpath = os.path.join(RESULTS_DIR, f"{tag}_report.json")
            with open(rpath, "w") as fh:
                json.dump(record, fh, indent=2)
            stats = out.report["statistics"]
            summary.append({
                "tag": tag,
                "cost_fp": out.cost_fp,
                "ticks": len(out.trajectory),
                "accum_err_fp": stats["cost_accumulation"]["max_error_fp"],
                "heur_over_fp":
                    stats["heuristic_deviation"]["max_overestimate_fp"],
                "smooth_diff_fp": stats["smoothing_cost"]["diff_fp"],
                "adv_resid_micro":
                    stats["advance_quantization"]["max_residual_micro"],
                "frame_dev_fp": fr["max_deviation_fp"],
                "violations": len(out.report["violations"]),
            })
            print(f"{tag}: cost={out.cost_fp} ticks={len(out.trajectory)} "
                  f"consistency=OK frame_dev={fr['max_deviation_fp']}"
                  f"/{fr['bound_fp']}fp")

    with open(os.path.join(RESULTS_DIR, "summary.json"), "w") as fh:
        json.dump(summary, fh, indent=2)
    bad = [s for s in summary if s["violations"]]
    print(f"\n{len(summary)} runs, all three-path comparisons OK, "
          f"bound violations: {len(bad)}")
    if bad:
        raise TacError("E_BOUND", "bound violations detected", bad)


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "all"
    if cmd in ("gen", "all"):
        cmd_gen()
    if cmd in ("run", "all"):
        cmd_run()
    if cmd not in ("gen", "run", "all"):
        print(__doc__)
        sys.exit(2)
