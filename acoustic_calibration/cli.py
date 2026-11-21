"""Command-line entry points for demo, verification and GUI launch."""

from __future__ import annotations

import argparse
import json
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np

from .engine import CalibrationEngine, CancellationToken
from .errors import CalibrationError, E_CANCELLED
from .hashing import to_jsonable
from .models import CalibrationParams, CalibrationState, TargetCurve
from .samples import generate_sample_wav
from .storage import atomic_write_json
from .versioning import StateStore


def _state_summary(state: CalibrationState) -> dict[str, Any]:
    return {
        "version": state.version,
        "content_hash": state.content_hash,
        "parent_hash": state.parent_hash,
        "layout": state.channel_names,
        "bands": len(state.bands),
        "arrivals": {
            channel.alignment.channel_name: channel.alignment.arrival_sample for channel in state.channels
        },
        "offsets": {
            channel.alignment.channel_name: channel.alignment.offset for channel in state.channels
        },
        "filter_taps": {channel.alignment.channel_name: len(channel.filter.coefficients) for channel in state.channels},
        "group_delay_ms": {
            channel.alignment.channel_name: round(channel.filter.group_delay_ms, 4) for channel in state.channels
        },
        "max_error_db": {
            channel.alignment.channel_name: round(max(error.upper_db for error in channel.errors), 4)
            for channel in state.channels
        },
        "affected_channels": list(state.affected_channels),
        "affected_bands": len(state.affected_bands),
        "recomputed_nodes": len(state.recomputed_nodes),
        "consistency_alerts": len(state.consistency_alerts),
        "alignment_diff": state.alignment_diff,
    }


def _artifacts_equal(left: CalibrationState, right: CalibrationState) -> bool:
    if left.channel_names != right.channel_names or len(left.bands) != len(right.bands):
        return False
    for left_channel, right_channel in zip(left.channels, right.channels, strict=True):
        if left_channel.alignment.arrival_sample != right_channel.alignment.arrival_sample:
            return False
        left_coefficients = np.asarray(left_channel.filter.coefficients, dtype=np.float64)
        right_coefficients = np.asarray(right_channel.filter.coefficients, dtype=np.float64)
        if left_coefficients.shape != right_coefficients.shape:
            return False
        if not np.array_equal(left_coefficients, right_coefficients):
            return False
        for left_band, right_band in zip(left_channel.spectral.bands, right_channel.spectral.bands, strict=True):
            if left_band.power != right_band.power or left_band.relative_upper != right_band.relative_upper:
                return False
        for left_error, right_error in zip(left_channel.errors, right_channel.errors, strict=True):
            if left_error.upper_db != right_error.upper_db:
                return False
    return True


def _make_target_at_band_centers(params: CalibrationParams, offset_db: float = 1.0) -> TargetCurve:
    from .spectral import make_bands

    bands = make_bands(params.spectral)
    points = [(band.center_hz, 0.0) for band in bands]
    points[len(points) // 2] = (points[len(points) // 2][0], offset_db)
    return TargetCurve(tuple(points))


def command_demo(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).resolve()
    source = workspace / "source" / "demo_5_1.wav"
    info = generate_sample_wav(source, layout="5.1", duration_s=args.duration, seed=args.seed)
    engine = CalibrationEngine(workspace / "measurement", source)
    params = CalibrationParams()
    if args.error_threshold is not None:
        params = replace(params, error_threshold_db=float(args.error_threshold))
    state = engine.submit(params)
    payload = {
        "sample": info.to_dict(),
        "state": _state_summary(state),
        "workspace": str(workspace),
    }
    print(json.dumps(to_jsonable(payload), ensure_ascii=False, indent=2))
    return 0


def command_verify(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).resolve()
    workspace.mkdir(parents=True, exist_ok=True)
    source = workspace / "source" / "acceptance_5_1.wav"
    info = generate_sample_wav(source, layout="5.1", duration_s=args.duration, seed=args.seed)
    engine = CalibrationEngine(workspace / "main", source)
    base_params = CalibrationParams()
    results: list[dict[str, Any]] = []

    v1 = engine.submit(base_params)
    results.append({"step": "baseline", "state": _state_summary(v1)})
    arrivals_match = tuple(channel.alignment.arrival_sample for channel in v1.channels) == info.arrivals
    if not arrivals_match:
        raise RuntimeError("direct-sound arrivals do not match deterministic ground truth")

    reference_params = replace(
        base_params,
        alignment=replace(
            base_params.alignment,
            reference_channel=2,
            threshold=replace(base_params.alignment.threshold, floor_dbfs=-55.0),
        ),
    )
    v2 = engine.submit(reference_params)
    results.append({"step": "reference_and_threshold", "state": _state_summary(v2)})
    if v2.alignment_diff.get("old_reference_channel") != 0 or v2.alignment_diff.get("new_reference_channel") != 2:
        raise RuntimeError("alignment difference summary is missing the reference transition")

    smooth_params = replace(reference_params, smoothing_fraction=1.0 / 6.0)
    v3 = engine.submit(smooth_params)
    results.append({"step": "smoothing", "state": _state_summary(v3)})
    if len(v3.affected_bands) >= len(v3.bands) and len(v3.bands) > 1:
        # A smoothing-bandwidth change is allowed to affect every band; the
        # important invariant is that spectral nodes are reused.
        if any(node.startswith("spectral:") for node in v3.recomputed_nodes):
            raise RuntimeError("smoothing change unexpectedly recomputed spectral nodes")

    target_params = replace(
        smooth_params,
        target_curve=_make_target_at_band_centers(smooth_params),
    )
    v4 = engine.submit(target_params)
    results.append({"step": "target_curve", "state": _state_summary(v4)})
    if any(node.startswith("smooth:") for node in v4.recomputed_nodes):
        raise RuntimeError("target-curve change unexpectedly recomputed smoothing nodes")

    threshold_params = replace(target_params, error_threshold_db=0.25)
    v5 = engine.submit(threshold_params)
    results.append({"step": "error_threshold", "state": _state_summary(v5)})
    filters_unchanged = all(
        left.filter.coefficients == right.filter.coefficients
        for left, right in zip(v4.channels, v5.channels, strict=True)
    )
    if not filters_unchanged:
        raise RuntimeError("error-threshold change did not retain filters")

    rollback = engine.rollback(1)
    results.append({"step": "rollback", "state": _state_summary(rollback)})
    if rollback.version != 6 or rollback.rollback_of != 1:
        raise RuntimeError("rollback was not appended as a new version")
    chain = engine.state_store.validate_chain()
    if len(chain) != 6:
        raise RuntimeError("version hash chain length is inconsistent")

    recovery_params = replace(target_params, smoothing_fraction=1.0 / 4.0)
    token = CancellationToken()

    def cancel_after_first_channel(event: dict[str, Any]) -> None:
        if event.get("stage") == "channel_complete" and event.get("completed") == 1:
            token.cancel()

    cancelled = False
    try:
        engine.submit(recovery_params, token=token, progress=cancel_after_first_channel)
    except CalibrationError as exc:
        if exc.code != E_CANCELLED:
            raise
        cancelled = True
    if not cancelled:
        raise RuntimeError("test cancellation did not reach a channel checkpoint")
    if engine.current_state().version != 6:
        raise RuntimeError("cancelled run altered the last published version")

    snapshots = engine.snapshot_manager.load_latest_valid()
    if snapshots is None or not snapshots.completed_channel_indices:
        raise RuntimeError("cancelled run did not leave a recoverable channel checkpoint")
    recovered_engine = CalibrationEngine(workspace / "main", source)
    recovery = recovered_engine.recover()
    results.append(
        {
            "step": "recovery",
            "state": _state_summary(recovery.state),
            "reused_channels": list(recovery.reused_channels),
            "recomputed_channels": list(recovery.recomputed_channels),
        }
    )
    if recovery.state.version != 7 or not recovery.reused_channels:
        raise RuntimeError("recovery did not append a version while reusing completed channels")

    clean_engine = CalibrationEngine(workspace / "clean", source)
    clean = clean_engine.submit(recovery_params)
    artifacts_equal = _artifacts_equal(recovery.state, clean)
    results.append({"step": "artifact_equality", "equal": artifacts_equal})
    if not artifacts_equal:
        raise RuntimeError("recovered artifacts differ from a clean full recomputation")

    payload = {
        "sample": info.to_dict(),
        "workspace": str(workspace),
        "results": results,
        "verification": {
            "arrivals_match_ground_truth": arrivals_match,
            "version_chain_valid": len(chain) == 6,
            "rollback_appended": rollback.rollback_of == 1,
            "forced_cancel_preserved_version": engine.current_state().version >= 6,
            "recovery_reused_channels": list(recovery.reused_channels),
            "recovery_recomputed_channels": list(recovery.recomputed_channels),
            "recovered_artifacts_equal_clean_run": artifacts_equal,
        },
    }
    print(json.dumps(to_jsonable(payload), ensure_ascii=False, indent=2))
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(workspace_root=args.workspace, wav_path=args.wav)


def _export_state(state: CalibrationState) -> dict[str, Any]:
    channels: list[dict[str, Any]] = []
    for channel in state.channels:
        channels.append(
            {
                "channel_index": channel.alignment.channel_index,
                "channel_name": channel.alignment.channel_name,
                "arrival_sample": channel.alignment.arrival_sample,
                "offset_samples": channel.alignment.offset,
                "gain": channel.alignment.gain,
                "threshold_linear": channel.alignment.threshold,
                "filter": {
                    "order": channel.filter.order,
                    "group_delay_samples": channel.filter.group_delay_samples,
                    "group_delay_ms": channel.filter.group_delay_ms,
                    "coefficients": list(channel.filter.coefficients),
                },
                "band_errors": [
                    {
                        "band_index": error.band_index,
                        "center_hz": state.bands[error.band_index].center_hz,
                        "upper_db": error.upper_db,
                        "threshold_crossed": error.threshold_crossed,
                        "dominant_node": error.dominant_node,
                        "attribution_node": error.attribution_node,
                        "nodes": [node.to_dict() for node in error.nodes],
                    }
                    for error in channel.errors
                ],
            }
        )
    return {
        "version": state.version,
        "content_hash": state.content_hash,
        "parent_hash": state.parent_hash,
        "params": state.params.to_dict(),
        "bands": [band.to_dict() for band in state.bands],
        "channels": channels,
        "consistency_alerts": [alert.to_dict() for alert in state.consistency_alerts],
    }


def command_export(args: argparse.Namespace) -> int:
    store = StateStore(Path(args.workspace).resolve() / "state")
    state = store.load(args.version) if args.version is not None else store.latest()
    if state is None:
        raise RuntimeError("no published calibration version exists")
    payload = _export_state(state)
    if args.output:
        atomic_write_json(Path(args.output), payload, mode=0o644)
    print(json.dumps(to_jsonable({k: v for k, v in payload.items() if k not in ("channels", "bands")}), ensure_ascii=False, indent=2))
    print(f"Exported {len(payload['channels'])} channels and {sum(len(channel['band_errors']) for channel in payload['channels'])} band errors.")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Multichannel acoustic calibration workbench")
    subparsers = parser.add_subparsers(dest="command", required=True)

    demo = subparsers.add_parser("demo", help="generate a deterministic sample and publish one version")
    demo.add_argument("--workspace", default="artifacts/demo")
    demo.add_argument("--duration", type=float, default=0.75)
    demo.add_argument("--seed", type=int, default=19093)
    demo.add_argument("--error-threshold", type=float, default=None)
    demo.set_defaults(func=command_demo)

    verify = subparsers.add_parser("verify", help="run the complete acceptance scenario")
    verify.add_argument("--workspace", default="artifacts/verification")
    verify.add_argument("--duration", type=float, default=0.75)
    verify.add_argument("--seed", type=int, default=19093)
    verify.set_defaults(func=command_verify)

    gui = subparsers.add_parser("gui", help="launch the PySide6 workbench")
    gui.add_argument("--workspace", default=None)
    gui.add_argument("--wav", default=None)
    gui.set_defaults(func=command_gui)

    export = subparsers.add_parser("export", help="export coefficients and error summaries for one version")
    export.add_argument("--workspace", required=True, help="calibration workspace containing state/")
    export.add_argument("--version", type=int, default=None)
    export.add_argument("--output", default="calibration_export.json")
    export.set_defaults(func=command_export)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
