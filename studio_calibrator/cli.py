from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import List, Optional

from .config import load_config, parse_config
from .engine import AnalysisEngine, RecomputeManager
from .errors import E_CONFIG_INVALID, BusinessError
from .geometry import load_geometry
from .measurement_io import load_manifest
from .samplegen import generate_sample
from .snapshot import SnapshotStore
from .util import canonical_json_bytes, load_json


def _write_output(payload: object, output: Optional[Path]) -> None:
    data = canonical_json_bytes(payload)
    if output is None:
        sys.stdout.buffer.write(data)
        return
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_bytes(data)


def _diagnose(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    manifest = load_manifest(args.manifest, config.measurement_layout)
    geometry = load_geometry(args.geometry)
    engine = AnalysisEngine(args.manifest, args.geometry)
    manager = RecomputeManager(engine, SnapshotStore(args.snapshot_dir))
    try:
        state = manager.recompute(config, args.trace)
    except BusinessError as exc:
        _write_output(
            {
                "ok": False,
                "error": exc.to_dict(),
                "last_snapshot": manager.last_snapshot,
            },
            args.out,
        )
        return 3
    summary = {
        "ok": state.status == "ok",
        "status": state.status,
        "measurement_layout": config.measurement_layout,
        "speakers": list(manifest.speakers),
        "receivers": list(manifest.receivers),
        "bands": list(config.band_ids),
        "pair_count": len(manifest.pairs),
        "alignment_confidence": {
            item["source"] + "|" + item["receiver"]: item["confidence"]
            for item in state.alignments
        },
        "coupling_diff": state.coupling_diff,
        "phase_summary": state.phase_summary,
        "budget_short": [
            {
                "band_id": band["band_id"],
                "pairs": band["short_bands"],
            }
            for band in state.budget
            if band["short_bands"]
        ],
        "calibration": {
            "feasible": state.calibration["feasible"],
            "publishable": state.calibration["publishable"],
            "conflict_count": len(state.calibration["conflicts"]),
            "setting_count": len(state.calibration["settings"]),
        },
        "errors": list(state.errors),
        "incremental": state.incremental,
        "node_hashes": state.node_hashes,
        "trace_hash": state.trace_hash,
        "state_hash": state.content_hash_value,
        "snapshot": manager.last_snapshot,
        "geometry_pairs": len(geometry.pairs),
    }
    _write_output(summary, args.out)
    if state.status == "ok":
        return 0
    if state.status == "degraded":
        return 4
    return 5


def _restore(args: argparse.Namespace) -> int:
    store = SnapshotStore(args.snapshot_dir)
    try:
        restored = store.load_current()
    except BusinessError as exc:
        _write_output({"ok": False, "error": exc.to_dict()}, args.out)
        return 3
    state = restored["state"]
    payload = {
        "ok": True,
        "snapshot_id": restored["snapshot_id"],
        "metadata": restored["metadata"],
        "status": state["status"],
        "state_hash": restored["metadata"]["state_hash"],
        "route_matrix_sha256": restored["metadata"]["route_matrix_sha256"],
        "calibration_sha256": restored["metadata"]["calibration_sha256"],
        "route_matrix": state["route_matrix"],
        "calibration": state["calibration"],
        "committed_ids": store.committed_ids(),
    }
    _write_output(payload, args.out)
    return 0


def _generate(args: argparse.Namespace) -> int:
    try:
        result = generate_sample(
            args.out, seed=args.seed, duration=args.duration, force=args.force
        )
    except BusinessError as exc:
        _write_output({"ok": False, "error": exc.to_dict()}, None)
        return 2
    _write_output({"ok": True, **result}, None)
    return 0


def _validate(args: argparse.Namespace) -> int:
    try:
        config = load_config(args.config)
        manifest = load_manifest(args.manifest, config.measurement_layout)
        geometry = load_geometry(args.geometry)
        nyquist = manifest.sample_rate / 2.0
        for band in config.bands:
            if band.high_hz > nyquist:
                raise BusinessError(
                    E_CONFIG_INVALID,
                    "band high_hz exceeds Nyquist frequency",
                    band=band.id,
                    details={"high_hz": band.high_hz, "nyquist_hz": nyquist},
                )
        payload = {
            "ok": True,
            "config_hash": config.hash,
            "bands": [band.to_dict() for band in config.bands],
            "measurement_pairs": len(manifest.pairs),
            "geometry_pairs": len(geometry.pairs),
            "target_suppression_db": config.target_suppression_db,
            "max_cancellation_gain_db": config.max_cancellation_gain_db,
        }
    except BusinessError as exc:
        _write_output({"ok": False, "error": exc.to_dict()}, None)
        return 2
    _write_output(payload, None)
    return 0


def _gui(args: argparse.Namespace) -> int:
    from .gui import launch

    return launch(
        manifest=args.manifest,
        geometry=args.geometry,
        config=args.config,
        snapshot_dir=args.snapshot_dir,
        trace_path=args.trace,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="studio-calibrator",
        description="Deterministic multichannel studio crosstalk calibration",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", required=True, type=Path)
    generate.add_argument("--seed", type=int, default=42)
    generate.add_argument("--duration", type=float, default=1.0)
    generate.add_argument("--force", action="store_true")
    generate.set_defaults(handler=_generate)

    validate = sub.add_parser("validate", help="validate config, geometry and manifest")
    validate.add_argument("--manifest", required=True, type=Path)
    validate.add_argument("--geometry", required=True, type=Path)
    validate.add_argument("--config", required=True, type=Path)
    validate.set_defaults(handler=_validate)

    diagnose = sub.add_parser("diagnose", help="run the deterministic analysis pipeline")
    diagnose.add_argument("--manifest", required=True, type=Path)
    diagnose.add_argument("--geometry", required=True, type=Path)
    diagnose.add_argument("--config", required=True, type=Path)
    diagnose.add_argument("--snapshot-dir", required=True, type=Path)
    diagnose.add_argument("--trace", type=Path)
    diagnose.add_argument("--out", type=Path)
    diagnose.set_defaults(handler=_diagnose)

    restore = sub.add_parser("restore", help="restore the last committed snapshot")
    restore.add_argument("--snapshot-dir", required=True, type=Path)
    restore.add_argument("--out", type=Path)
    restore.set_defaults(handler=_restore)

    gui = sub.add_parser("gui", help="start the PyQt5 workbench")
    gui.add_argument("--manifest", required=True, type=Path)
    gui.add_argument("--geometry", required=True, type=Path)
    gui.add_argument("--config", required=True, type=Path)
    gui.add_argument("--snapshot-dir", required=True, type=Path)
    gui.add_argument("--trace", type=Path)
    gui.set_defaults(handler=_gui)
    return parser


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)
    return int(args.handler(args))


if __name__ == "__main__":
    raise SystemExit(main())
