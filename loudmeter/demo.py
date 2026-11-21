"""Scripted deterministic demo entry.

Runs the full chain on local synthetic samples: WAV read -> downmix ->
block slicing -> K-weighted block metering -> two-stage gating -> parallel
scheduling -> parameter-change recompute -> versioned publish -> atomic
snapshot -> forced-termination recovery. Writes trace.jsonl and
report.json into the work directory. No wall-clock time is used anywhere;
sync markers are logical sequence numbers, so identical seeds and
operation sequences give byte-identical outputs.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

from .config import MeterConfig
from .gating import compute_gating
from .pipeline import build_manifest, meter_asset
from .recompute import plan_recompute
from .samplegen import generate_all
from .scheduler import Scheduler, TraceLog
from .snapshot import SnapshotStore, unfinished_blocks
from .versions import VersionStore
from .wavfile import read_wav


def _dump_jsonl(path: Path, entries: list[dict]) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        for e in entries:
            fh.write(json.dumps(e, sort_keys=True, ensure_ascii=False,
                                allow_nan=False))
            fh.write("\n")


def _dump_json(path: Path, obj) -> None:
    path.write_text(json.dumps(obj, sort_keys=True, ensure_ascii=False,
                               indent=2, allow_nan=False) + "\n",
                    encoding="utf-8")


def run(workdir: str | Path) -> dict:
    workdir = Path(workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    (workdir / "samples").mkdir(exist_ok=True)

    config = MeterConfig()
    _dump_json(workdir / "config.json", config.to_dict())
    assets = generate_all(workdir / "samples")

    trace = TraceLog()
    store = VersionStore()
    report = {"assets": {}, "recompute": [], "recovery": {}, "versions": []}

    # -- 1) parallel block metering across assets -------------------------
    from .blocks import measure_block, slice_blocks

    sched = Scheduler(config, trace)
    wavs = {}
    for a in assets:
        wav = read_wav(a["path"])
        wavs[a["asset_id"]] = wav
        fns = []
        for w in slice_blocks(wav.n_frames, config.block_samples):
            def make(w=w, wav=wav, aid=a["asset_id"]):
                return lambda: measure_block(
                    aid, w, w.index, wav.samples[w.start:w.end],
                    config.channel_weights, config.oversample_factor)
            fns.append((w.index, make()))
        sched.submit_asset(a["asset_id"], fns)
    sched.run()

    # -- 2) gating + versioned publish + block-level trace ----------------
    block_trace = []
    dispatch_info = {}
    for e in trace.entries:
        if e["event"] == "dispatch":
            dispatch_info[(e["asset_id"], e["block_index"])] = e
    for a in assets:
        aid = a["asset_id"]
        blocks = [sched.results[(aid, i)] for i in sorted(
            i for (x, i) in sched.results if x == aid)]
        gating = compute_gating(blocks, config.abs_gate_lufs,
                                config.rel_gate_offset_lu)
        gated = set(gating.selected_block_ids)
        for b in blocks:
            d = dispatch_info[(aid, b.index)]
            block_trace.append({
                "tick": d["tick"],
                "event": "block",
                "asset_id": aid,
                "block_index": b.index,
                "sync_marker": b.sync_marker,
                "short_term_lufs": b.short_term_lufs,
                "gated_in": b.index in gated,
                "true_peak_dbtp": b.true_peak_dbtp,
                "parallelism": d["parallelism"],
                "budget_used": d["budget_used"],
                "budget_total": d["budget_total"],
                "content_hash": b.content_hash,
            })
        manifest = build_manifest(aid, config, blocks, params_version=1)
        handle = store.publish(manifest)
        result = store.query(handle)
        report["assets"][aid] = {
            "version_hash": result.version_hash,
            "handle": handle,
            "integrated_lufs": manifest["gating"]["integrated_lufs"],
            "true_peak_max_dbtp": manifest["gating"]["true_peak_max_dbtp"],
            "gated_out_block_ids": manifest["gating"]["gated_out_block_ids"],
            "block_count": len(blocks),
        }
        report["versions"].append({"asset_id": aid, "handle": handle,
                                   "version_hash": result.version_hash})

    # -- 3) parameter change -> gating-aware recompute --------------------
    # 3a) gate change only (incremental path) on gated24
    aid = "gated24"
    old_blocks = [sched.results[(aid, i)] for i in sorted(
        i for (x, i) in sched.results if x == aid)]
    old_gating = compute_gating(old_blocks, config.abs_gate_lufs,
                                config.rel_gate_offset_lu)
    cfg_gate = config.update(rel_gate_offset_lu=-8.0,
                             target_loudness_lufs=-23.0)
    plan, new_blocks, new_gating = plan_recompute(
        aid, wavs[aid].samples, config, cfg_gate, old_blocks, old_gating)
    manifest = build_manifest(aid, cfg_gate, new_blocks, params_version=2)
    handle = store.publish(manifest)
    report["recompute"].append({
        "asset_id": aid, "change": "rel_gate_offset -10 -> -8",
        "mode": plan.mode, "reason": plan.reason,
        "affected_block_ids": list(plan.affected_block_ids),
        "gating_selection_changed": plan.gating_selection_changed,
        "version_hash": store.query(handle).version_hash,
    })

    # 3b) block-length change (full re-slice path) on tone16
    aid = "tone16"
    old_blocks = [sched.results[(aid, i)] for i in sorted(
        i for (x, i) in sched.results if x == aid)]
    old_gating = compute_gating(old_blocks, config.abs_gate_lufs,
                                config.rel_gate_offset_lu)
    cfg_slice = config.update(block_samples=9600)
    plan, new_blocks, new_gating = plan_recompute(
        aid, wavs[aid].samples, config, cfg_slice, old_blocks, old_gating)
    manifest = build_manifest(aid, cfg_slice, new_blocks, params_version=3)
    handle = store.publish(manifest)
    report["recompute"].append({
        "asset_id": aid, "change": "block_samples 19200 -> 9600",
        "mode": plan.mode, "reason": plan.reason,
        "reslice": plan.reslice,
        "gating_selection_changed": plan.gating_selection_changed,
        "version_hash": store.query(handle).version_hash,
    })

    # -- 4) atomic snapshot + forced-termination recovery -----------------
    aid = "gated24"
    snap_store = SnapshotStore(workdir / "snapshots")
    wav = wavs[aid]
    total = len(slice_blocks(wav.n_frames, config.block_samples))
    # first run: forced termination after 2 committed blocks
    part = meter_asset(aid, wav.samples, config, stop_after=2)
    snap_store.save({
        "params": config.to_dict(),
        "committed_blocks": [b.to_dict() for b in part],
        "gating": None,
    })
    # "restart": resume from the latest valid snapshot
    state = snap_store.load_latest()
    committed = [b["index"] for b in state["committed_blocks"]]
    rest = meter_asset(aid, wav.samples, config,
                       committed=tuple(committed),
                       stop_after=None,
                       sync_start=0)
    from .blocks import BlockMeasurement
    resumed_blocks = [BlockMeasurement(**b) for b in state["committed_blocks"]] + rest
    resumed_blocks.sort(key=lambda b: b.index)
    # reference: uninterrupted full run
    full_blocks = meter_asset(aid, wav.samples, config)
    m_resume = build_manifest(aid, config, resumed_blocks, params_version=1)
    m_full = build_manifest(aid, config, full_blocks, params_version=1)
    from .versions import version_hash
    report["recovery"] = {
        "asset_id": aid,
        "total_blocks": total,
        "committed_before_crash": len(part),
        "unfinished_after_recovery": unfinished_blocks(state, total),
        "resumed_version_hash": version_hash(m_resume),
        "full_run_version_hash": version_hash(m_full),
        "consistent": version_hash(m_resume) == version_hash(m_full),
    }

    # -- 5) write deterministic outputs -----------------------------------
    _dump_jsonl(workdir / "trace.jsonl", trace.entries)
    _dump_jsonl(workdir / "blocks.jsonl", block_trace)
    _dump_json(workdir / "report.json", report)
    return report


def main(argv=None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    workdir = argv[0] if argv else "workdir"
    report = run(workdir)
    print(json.dumps({"workdir": str(workdir),
                      "assets": list(report["assets"]),
                      "recovery_consistent": report["recovery"]["consistent"]},
                     ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
