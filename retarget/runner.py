"""Run / resume / finalize orchestration and consistency validation."""

import math
import os
import time

from .checkpoint import CheckpointManager
from .errors import E_RECOVERY_INVALID, E_RUN_INCOMPLETE, RetargetError
from .formats import load_target_skeleton, parse_animation
from .mapping import build_mapping
from .pipeline import Config, Precompute, compute_bind_alignment, \
    compute_interval
from .quat import qangle_between
from .util import content_hash, hash_file, to_py, write_json


def _run_config_dict(anim_path, target_path, config, anim, target):
    from .util import hash_file as _hf
    return {
        "anim_path": anim_path,
        "target_path": target_path,
        "anim_hash": _hf(anim_path),
        "target_hash": _hf(target_path),
        "params": config.to_dict(),
        "total_frames": anim.frame_count,
        "interval_frames": config.interval_frames,
        "format_version": anim.version,
    }


def _build_header(anim_path, target_path, config):
    anim = parse_animation(anim_path)
    target = load_target_skeleton(target_path)
    mapping = build_mapping(anim, target)
    pre = Precompute(anim, target, mapping)
    align, bind_events = compute_bind_alignment(pre, config)
    bind_doc = {
        "alignment": {
            t: {path: to_py(q) for path, q in
                (("float", align["float"][t]), ("fixed", align["fixed"][t]))}
            for t in pre.tgt_to_src
        },
        "quant_events": bind_events,
    }
    header = {
        "run_config": _run_config_dict(anim_path, target_path, config,
                                       anim, target),
        "mapping": mapping,
        "mapping_hash": content_hash(mapping),
        "bind_alignment": bind_doc,
        "bind_hash": content_hash(bind_doc),
    }
    return header, anim, target, mapping, pre, align


def _compute_and_commit(ckpt, header, pre, config, align, start_interval):
    total = pre.anim.frame_count
    n_frames = config.interval_frames
    n_intervals = (total + n_frames - 1) // n_frames
    if start_interval == 0:
        prev = ckpt.init_run(header)
    else:
        _, manifest, intervals = ckpt.load_verified()
        prev = manifest["head_hash"]
    for idx in range(start_interval, n_intervals):
        f0 = idx * n_frames
        f1 = min(total, f0 + n_frames)
        payload = compute_interval(pre, config, align, f0, f1)
        prev = ckpt.commit_interval(idx, payload, prev)
        if config.interval_delay_ms:
            time.sleep(config.interval_delay_ms / 1000.0)
    ckpt.mark_complete()


def run(anim_path, target_path, out_dir, config):
    """Fresh run: parse, map, compute all intervals, finalize."""
    ckpt = CheckpointManager(out_dir)
    header, anim, target, mapping, pre, align = _build_header(
        anim_path, target_path, config)
    _compute_and_commit(ckpt, header, pre, config, align, 0)
    finalize(out_dir)
    return header


def resume(out_dir, interval_delay_ms=0):
    """Recover from disk snapshots only; recompute unfinished intervals.

    Recovery never writes the exit report (report.json / trajectory.log /
    poses files); use `finalize` after a successful resume.
    """
    ckpt = CheckpointManager(out_dir)
    header, manifest, _ = ckpt.load_verified()
    rc = header["run_config"]
    config = Config.from_dict(rc["params"])
    config.interval_delay_ms = interval_delay_ms
    anim = parse_animation(rc["anim_path"])
    target = load_target_skeleton(rc["target_path"])
    mapping = build_mapping(anim, target)
    if content_hash(mapping) != header["mapping_hash"]:
        raise RetargetError(E_RECOVERY_INVALID,
                            "recomputed mapping does not match header",
                            {"first_broken_frame": 0})
    pre = Precompute(anim, target, mapping)
    align, bind_events = compute_bind_alignment(pre, config)
    bind_doc = {
        "alignment": {
            t: {path: to_py(q) for path, q in
                (("float", align["float"][t]), ("fixed", align["fixed"][t]))}
            for t in pre.tgt_to_src
        },
        "quant_events": bind_events,
    }
    if content_hash(bind_doc) != header["bind_hash"]:
        raise RetargetError(E_RECOVERY_INVALID,
                            "recomputed bind alignment does not match header",
                            {"first_broken_frame": 0})
    start = manifest["committed_intervals"]
    _compute_and_commit(ckpt, header, pre, config, align, start)
    return {"resumed_from_interval": start,
            "total_frames": rc["total_frames"]}


def finalize(out_dir):
    """Assemble poses, trajectory log and the exit report from committed
    intervals. This is the only stage that writes final artifacts."""
    ckpt = CheckpointManager(out_dir)
    header, manifest, intervals = ckpt.load_verified()
    rc = header["run_config"]
    if not manifest["complete"]:
        raise RetargetError(
            E_RUN_INCOMPLETE,
            "run is incomplete; execute `resume` before `finalize`",
            {"committed_intervals": manifest["committed_intervals"]})
    config = Config.from_dict(rc["params"])
    paths = config.paths()
    mapping = header["mapping"]
    method_of = {m["target"]: m["method"] for m in mapping["mappings"]}
    mapped_targets = [m["target"] for m in mapping["mappings"]]

    # Assemble per-frame poses and trajectory log from interval snapshots.
    frame_poses = {p: [] for p in paths}
    error_records = {p: [] for p in paths}
    scale_records = {p: [] for p in paths}
    quant_events = list(header["bind_alignment"]["quant_events"])
    for doc in intervals:
        for p in paths:
            frame_poses[p].extend(doc["poses"][p])
            error_records[p].extend(doc["error_records"][p])
            scale_records[p].extend(doc["scale_records"][p])
        quant_events.extend(doc["quant_events"])

    os.makedirs(out_dir, exist_ok=True)
    pose_files = {}
    for p in paths:
        path = os.path.join(out_dir, f"poses_{p}.jsonl")
        with open(path, "w", encoding="utf-8") as fh:
            for fp in frame_poses[p]:
                fh.write(_line(fp))
        pose_files[p] = path

    # Consistency validation: FK-recomputed target global orientation vs
    # the source-truth trajectory mapped through bind alignment.
    tolerance = config.tolerance_deg
    path_reports = {}
    for p in paths:
        joints_stats = {t: {"max_deg": 0.0, "sum_deg": 0.0, "count": 0}
                        for t in mapped_targets}
        worst = {"deg": -1.0, "joint": None, "frame": None}
        exceedances = []
        bound_lookup = {}
        for rec in error_records[p]:
            bound_lookup[(rec["frame"], rec["joint"])] = rec
        scale_lookup = {}
        for rec in scale_records[p]:
            scale_lookup.setdefault(rec["frame"], {})[rec["joint"]] = rec
        for fp in frame_poses[p]:
            f = fp["frame"]
            for t in mapped_targets:
                dev = math.degrees(qangle_between(
                    fp["truth_global_rot"][t], fp["global_rot"][t]))
                st = joints_stats[t]
                st["max_deg"] = max(st["max_deg"], dev)
                st["sum_deg"] += dev
                st["count"] += 1
                if dev > worst["deg"]:
                    worst = {"deg": dev, "joint": t, "frame": f}
                if dev > tolerance:
                    exceedances.append(_attribute_exceedance(
                        p, f, t, dev, bound_lookup.get((f, t)),
                        scale_lookup.get(f, {}), config))
        joints_out = {
            t: {"max_deg": st["max_deg"],
                "mean_deg": (st["sum_deg"] / st["count"]
                             if st["count"] else 0.0)}
            for t, st in joints_stats.items()
        }
        path_reports[p] = {
            "verdict": "pass" if worst["deg"] <= tolerance else "fail",
            "tolerance_deg": tolerance,
            "max_deviation": worst,
            "joints": joints_out,
            "exceedances": exceedances,
            "untrusted_marks": sum(
                1 for r in error_records[p] if r["untrusted"]),
            "unknown_attributions": sum(
                1 for r in error_records[p]
                if r["attribution"]["status"] == "unknown"),
        }

    # Float vs fixed divergence (the original outsourcing-team symptom).
    divergence = {"max_deg": 0.0, "joint": None, "frame": None}
    if len(paths) == 2:
        fa, fb = frame_poses["float"], frame_poses["fixed"]
        for i in range(len(fa)):
            for t in mapped_targets:
                dev = math.degrees(qangle_between(
                    fa[i]["global_rot"][t], fb[i]["global_rot"][t]))
                if dev > divergence["max_deg"]:
                    divergence = {"max_deg": dev, "joint": t,
                                  "frame": fa[i]["frame"]}

    # Trajectory log: frame, version, mapping method, error bound,
    # untrusted flag, content hash. No timestamps, no file sizes.
    log_path = os.path.join(out_dir, "trajectory.log")
    bound_by = {}
    for p in paths:
        for rec in error_records[p]:
            bound_by[(p, rec["frame"], rec["joint"])] = rec
    with open(log_path, "w", encoding="utf-8") as fh:
        for i, fp in enumerate(frame_poses[paths[0]]):
            f = fp["frame"]
            frame_hash = content_hash(
                {p: frame_poses[p][i] for p in paths})
            for t in mapped_targets:
                rec = bound_by[(paths[-1], f, t)]
                fh.write(_line({
                    "frame": f,
                    "version": rc["format_version"],
                    "joint": t,
                    "mapping": method_of[t],
                    "error_bound_deg": rec["bound_deg"],
                    "untrusted": rec["untrusted"],
                    "content_hash": frame_hash,
                }))

    scale_summary = {
        p: {
            "max_apply_diff": max(
                (r["apply_diff"] for r in scale_records[p]), default=0.0),
            "max_declared_diff": max(
                (r["declared_diff"] for r in scale_records[p]), default=0.0),
        }
        for p in paths
    }

    report = {
        "run_config": rc,
        "mapping_stats": mapping["stats"],
        "unmatched": mapping["unmatched"],
        "paths": path_reports,
        "float_fixed_divergence": divergence,
        "scale": scale_summary,
        "quant_events_total": len(quant_events),
        "hashes": {
            "poses": {p: hash_file(pose_files[p]) for p in paths},
            "trajectory_log": hash_file(log_path),
            "chain_head": manifest["head_hash"],
        },
    }
    write_json(os.path.join(out_dir, "report.json"), report)
    write_json(os.path.join(out_dir, "mapping_report.json"), mapping)
    return report


def _attribute_exceedance(path, frame, joint, dev_deg, bound_rec,
                          frame_scales, config):
    """Explain an over-tolerance deviation as quantization, scale or
    bind-alignment originating."""
    dev_rad = math.radians(dev_deg)
    cause = "bind_alignment"
    if frame_scales:
        # Only a genuine misapplication (actual vs intended apply diff,
        # e.g. double-applied scale) counts as a scale-originated error;
        # declared_diff from fixed-point coefficient quantization does not.
        worst_scale = max((r["apply_diff"]
                           for r in frame_scales.values()), default=0.0)
        if worst_scale > config.scale_tolerance:
            cause = "scale"
    if cause == "bind_alignment" and path == "fixed" and bound_rec:
        quant_bound = (bound_rec["channels"].get("quantization", 0.0)
                       + bound_rec["channels"].get("bind_alignment", 0.0))
        if quant_bound >= dev_rad:
            cause = "quantization"
    return {"frame": frame, "joint": joint, "deviation_deg": dev_deg,
            "cause": cause}


def _line(obj):
    import json
    return json.dumps(to_py(obj), sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False) + "\n"
