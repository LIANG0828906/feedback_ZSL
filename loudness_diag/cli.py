"""Scriptable diagnostic entry point."""

from __future__ import annotations

import argparse
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from .batch import run_batch
from .blocks import (
    analyze_asset,
    compare_slicing,
    finalize_report,
    make_block_evaluator,
    slice_blocks,
)
from .errors import (
    E_BUSY,
    E_CONFIG_INVALID,
    E_HEADER_INVALID,
    E_RECOVERY_INVALID,
    E_SNAPSHOT_RELEASED,
    E_VERSION_MISMATCH,
    DiagnosticError,
)
from .gate import GateParameters, apply_gate
from .generator import generate_test_set
from .jsonutil import atomic_write_bytes, canonical_bytes, load_json, sha256_json
from .scheduler import (
    BlockScheduler,
    SchedulerConfig,
    load_scheduler_config,
)
from .snapshot import SnapshotStore, snapshot_parameter_version
from .version_store import VersionedStore
from .wav import read_wav

_EXIT_CODES = {
    E_HEADER_INVALID: 2,
    E_BUSY: 3,
    E_CONFIG_INVALID: 4,
    E_VERSION_MISMATCH: 5,
    E_RECOVERY_INVALID: 6,
    E_SNAPSHOT_RELEASED: 7,
}


def _emit(value: Any) -> None:
    sys.stdout.buffer.write(canonical_bytes(value))
    sys.stdout.buffer.flush()


def _artifact_summary(command: str, **values: Any) -> dict[str, Any]:
    return {"ok": True, "command": command, **values}


def _write_report_and_trace(
    output_dir: Path, report: dict[str, Any], trace: list[dict[str, Any]]
) -> tuple[Path, Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    report_path = output_dir / "report.json"
    trace_path = output_dir / "trace.log"
    atomic_write_bytes(report_path, canonical_bytes(report))
    trace_bytes = b"".join(canonical_bytes(event) for event in trace)
    atomic_write_bytes(trace_path, trace_bytes)
    return report_path, trace_path


def _block_trace_events(
    report: dict[str, Any], scheduler: SchedulerConfig
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for row in report["blocks"]:
        events.append(
            {
                "event": "block_evaluated",
                "asset_id": report["asset_id"],
                "block_index": row["block_index"],
                "block_id": row["block_id"],
                "window_start": row["window_start"],
                "window_end": row["window_end"],
                "sync_marker": row["sync_marker"],
                "content_hash": row["content_hash"],
                "ungated_energy": row["ungated_energy"],
                "short_term_loudness": row["short_term_loudness"],
                "true_peak": row["true_peak"],
                "absolute_pass": row["absolute_pass"],
                "relative_pass": row["relative_pass"],
                "final_pass": row["final_pass"],
                "removed_reason": row["removed_reason"],
                "parallelism": scheduler.parallelism,
                "memory_budget_bytes": scheduler.memory_budget_bytes,
                "memory_used_bytes": scheduler.block_memory_bytes,
            }
        )
    return events


def _resolved_config(
    config_path: str | None, *, block_samples: int, channels: int
) -> SchedulerConfig:
    if config_path is not None:
        return load_scheduler_config(config_path)
    estimate = max(1, block_samples * channels * 16)
    return SchedulerConfig(
        parallelism=1,
        memory_budget_bytes=estimate,
        block_memory_bytes=estimate,
    )


def _gate_from_args(args: argparse.Namespace) -> GateParameters:
    return GateParameters(
        absolute_lufs=args.absolute_lufs,
        relative_offset_lu=args.relative_offset_lu,
        true_peak_limit_dbtp=args.true_peak_limit,
    )


def _snapshot_analysis(
    wav: Any,
    *,
    args: argparse.Namespace,
    config: SchedulerConfig,
    gate: GateParameters,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    specs = slice_blocks(wav, args.block_samples, asset_id=args.asset_id)
    evaluator = make_block_evaluator(
        wav,
        specs,
        oversample=args.oversample,
        block_samples=args.block_samples,
    )
    parameters = snapshot_parameter_version(
        block_samples=args.block_samples,
        oversample=args.oversample,
        gate_parameters=gate.as_dict(),
    )
    snapshot = SnapshotStore(
        args.snapshot_root,
        asset_id=args.asset_id,
        parameters=parameters,
        source_content_hash=wav.content_hash,
        parameter_version="gate-v1",
        total_blocks=len(specs),
    )
    committed_before = snapshot.committed_count
    computed_indices = list(range(committed_before, len(specs)))
    scheduler_trace: list[dict[str, Any]] = []
    if computed_indices:
        with BlockScheduler(config) as scheduler:
            computed_rows, scheduler_trace = scheduler.run(
                args.asset_id, computed_indices, evaluator
            )
        for row in computed_rows:
            snapshot.commit_block(row)
    stored_rows = [
        _public_block(row) for row in snapshot.block_records
    ]
    applied = apply_gate(stored_rows, gate)
    gate_result = {key: value for key, value in applied.items() if key != "blocks"}
    if snapshot.gate_result is None:
        snapshot.commit_gate_result(dict(applied))
    else:
        existing_core = {
            key: value
            for key, value in snapshot.gate_result.items()
            if key
            not in {"schema_version", "committed_block_count", "head_hash"}
        }
        if sha256_json(existing_core) != sha256_json(applied):
            raise DiagnosticError(
                E_RECOVERY_INVALID,
                "snapshot gate result does not match current block decisions",
            )
    report = {
        "schema_version": 1,
        "engine_version": "loudness-diag/1.0.0",
        "asset_id": args.asset_id,
        "asset": wav.evidence(),
        "analysis": {
            "block_samples": args.block_samples,
            "block_count": len(specs),
            "oversample": args.oversample,
            "synchronization": "sample_position",
            "scheduler": config.as_dict(),
        },
        "gate": gate_result,
        "blocks": applied["blocks"],
        "scheduler_trace": scheduler_trace,
        "decision_index": {
            "absolute_threshold_energy": gate_result["absolute_threshold_energy"],
            "relative_threshold_energy": gate_result["relative_threshold_energy"],
            "block_energy_field": "ungated_energy",
            "decision_fields": [
                "absolute_pass",
                "relative_pass",
                "final_pass",
                "removed_reason",
            ],
        },
        "decision_by_block_id": {
            row["block_id"]: {
                "ungated_energy": row["ungated_energy"],
                "absolute_pass": row["absolute_pass"],
                "relative_pass": row["relative_pass"],
                "final_pass": row["final_pass"],
                "removed_reason": row["removed_reason"],
            }
            for row in applied["blocks"]
        },
        "recovery": {
            "committed_before": committed_before,
            "computed_block_indices": computed_indices,
            "resumed_from_disk": committed_before > 0,
        },
    }
    report = finalize_report(report, oversample=args.oversample)
    trace = scheduler_trace + _block_trace_events(report, config)
    trace.append(
        {
            "event": "recovery",
            "asset_id": args.asset_id,
            "committed_before": committed_before,
            "committed_after": len(stored_rows),
            "computed_block_indices": computed_indices,
            "snapshot_directory": snapshot.directory.name,
        }
    )
    return report, trace


def _public_block(row: dict[str, Any]) -> dict[str, Any]:
    return {
        key: value
        for key, value in row.items()
        if key not in {"record_hash", "prev_hash"}
    }


def _command_generate(args: argparse.Namespace) -> dict[str, Any]:
    manifest = generate_test_set(args.output, seed=args.seed, seconds=args.seconds)
    return _artifact_summary(
        "generate",
        seed=args.seed,
        assets=len(manifest["assets"]),
        manifest="manifest.json",
    )


def _command_diagnose(args: argparse.Namespace) -> dict[str, Any]:
    wav = read_wav(args.wav, source_id=Path(args.wav).name)
    output_dir = Path(args.output)
    gate = _gate_from_args(args)
    config = _resolved_config(
        args.config, block_samples=args.block_samples, channels=wav.info.channels
    )
    if args.snapshot_root:
        report, trace = _snapshot_analysis(
            wav,
            args=args,
            config=config,
            gate=gate,
        )
    else:
        report, scheduler_trace = analyze_asset(
            wav,
            block_samples=args.block_samples,
            oversample=args.oversample,
            gate_parameters=gate,
            asset_id=args.asset_id,
            scheduler_config=config,
        )
        trace = scheduler_trace + _block_trace_events(report, config)
    report_path, trace_path = _write_report_and_trace(output_dir, report, trace)
    version_hash = None
    if args.versions_root:
        store = VersionedStore(args.versions_root)
        version_hash = store.publish(report)
    return _artifact_summary(
        "diagnose",
        asset_id=args.asset_id,
        report_hash=report["report_hash"],
        version_hash=version_hash,
        artifacts={"report": report_path.name, "trace": trace_path.name},
    )


def _load_combinations(path: str) -> list[GateParameters]:
    value = load_json(path)
    if isinstance(value, dict):
        value = value.get("combinations")
    if not isinstance(value, list) or not value:
        raise DiagnosticError(
            E_CONFIG_INVALID, "parameter file must contain a non-empty list"
        )
    combinations: list[GateParameters] = []
    for index, item in enumerate(value):
        if not isinstance(item, dict):
            raise DiagnosticError(
                E_CONFIG_INVALID, f"parameter combination {index} is not an object"
            )
        try:
            combinations.append(
                GateParameters(
                    absolute_lufs=float(item.get("absolute_lufs", -70.0)),
                    relative_offset_lu=float(item.get("relative_offset_lu", -10.0)),
                    true_peak_limit_dbtp=float(
                        item.get("true_peak_limit_dbtp", -1.0)
                    ),
                )
            )
        except (TypeError, ValueError) as exc:
            raise DiagnosticError(
                E_CONFIG_INVALID,
                f"parameter combination {index} is invalid: {exc}",
            ) from exc
    return combinations


def _command_batch(args: argparse.Namespace) -> dict[str, Any]:
    wav = read_wav(args.wav, source_id=Path(args.wav).name)
    config = _resolved_config(
        args.config, block_samples=args.block_samples, channels=wav.info.channels
    )
    base_report, scheduler_trace = analyze_asset(
        wav,
        block_samples=args.block_samples,
        oversample=args.oversample,
        gate_parameters=GateParameters(),
        asset_id=args.asset_id,
        scheduler_config=config,
    )
    combinations = _load_combinations(args.params)
    report = run_batch(
        base_report["blocks"],
        combinations,
        asset_id=args.asset_id,
        source_content_hash=wav.content_hash,
        analysis=base_report["analysis"],
    )
    report["block_energy_evidence"] = [
        {
            "block_id": row["block_id"],
            "ungated_energy": row["ungated_energy"],
            "true_peak": row["true_peak"],
            "content_hash": row["content_hash"],
        }
        for row in base_report["blocks"]
    ]
    report["report_hash"] = sha256_json(
        {key: value for key, value in report.items() if key != "report_hash"}
    )
    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)
    report_path = output_dir / "batch_report.json"
    trace_path = output_dir / "trace.log"
    atomic_write_bytes(report_path, canonical_bytes(report))
    trace = scheduler_trace + _block_trace_events(base_report, config)
    for result in report["results"]:
        combo_id = result["combo_id"]
        for block_id in report["block_ids"]:
            state = report["status_matrix"][block_id][combo_id]
            trace.append(
                {
                    "event": "batch_block_state",
                    "asset_id": args.asset_id,
                    "combo_id": combo_id,
                    "block_id": block_id,
                    "absolute_pass": state["absolute_pass"],
                    "relative_pass": state["relative_pass"],
                    "final_pass": state["final_pass"],
                    "true_peak_pass": state["true_peak_pass"],
                    "compliance_pass": state["compliance_pass"],
                    "removed_reason": state["removed_reason"],
                }
            )
    atomic_write_bytes(
        trace_path, b"".join(canonical_bytes(event) for event in trace)
    )
    return _artifact_summary(
        "batch",
        asset_id=args.asset_id,
        report_hash=report["report_hash"],
        combination_count=len(combinations),
        flipped_block_ids=report["flipped_block_ids"],
        artifacts={"report": report_path.name, "trace": trace_path.name},
    )


def _command_reslice(args: argparse.Namespace) -> dict[str, Any]:
    wav = read_wav(args.wav, source_id=Path(args.wav).name)
    config = _resolved_config(
        args.config,
        block_samples=max(args.old_block_samples, args.new_block_samples),
        channels=wav.info.channels,
    )
    comparison = compare_slicing(
        wav,
        old_block_samples=args.old_block_samples,
        new_block_samples=args.new_block_samples,
        old_oversample=args.old_oversample,
        new_oversample=args.new_oversample,
        asset_id=args.asset_id,
        gate_parameters=_gate_from_args(args),
        scheduler_config=config,
    )
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    atomic_write_bytes(output, canonical_bytes(comparison))
    return _artifact_summary(
        "reslice",
        block_count_delta=comparison["block_count_delta"],
        artifact=str(output.name),
    )


def _command_publish(args: argparse.Namespace) -> dict[str, Any]:
    report = load_json(args.report)
    store = VersionedStore(args.versions_root)
    version_hash = store.publish(report)
    return _artifact_summary(
        "publish",
        version_hash=version_hash,
        current_version_hash=store.current_version_hash,
    )


def _command_query(args: argparse.Namespace) -> dict[str, Any]:
    store = VersionedStore(args.versions_root)
    with store.query(args.version) as snapshot:
        payload = {
            "ok": True,
            "command": "query",
            "version_hash": snapshot.version_hash,
            "manifest_hash": snapshot.manifest_hash,
            "report": snapshot.report,
        }
    if args.output:
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        atomic_write_bytes(output, canonical_bytes(payload))
    return payload


def _command_recover(args: argparse.Namespace) -> dict[str, Any]:
    result = _command_diagnose(args)
    result["command"] = "recover"
    return result


def _add_analysis_arguments(
    parser: argparse.ArgumentParser, *, snapshot_required: bool = False
) -> None:
    parser.add_argument("--wav", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--asset-id", required=True)
    parser.add_argument("--block-samples", type=int, default=19200)
    parser.add_argument("--oversample", type=int, default=4)
    parser.add_argument("--absolute-lufs", type=float, default=-70.0)
    parser.add_argument("--relative-offset-lu", type=float, default=-10.0)
    parser.add_argument("--true-peak-limit", type=float, default=-1.0)
    parser.add_argument("--config")
    parser.add_argument("--snapshot-root", required=snapshot_required)
    parser.add_argument("--versions-root")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="loudness-diag",
        description="Block-domain 5.1 loudness diagnostics",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate")
    generate.add_argument("--output", required=True)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument("--seconds", type=float, default=4.0)
    generate.set_defaults(handler=_command_generate)

    diagnose = subparsers.add_parser("diagnose")
    _add_analysis_arguments(diagnose)
    diagnose.set_defaults(handler=_command_diagnose)

    recover = subparsers.add_parser("recover")
    _add_analysis_arguments(recover, snapshot_required=True)
    recover.set_defaults(handler=_command_recover)

    batch = subparsers.add_parser("batch")
    _add_analysis_arguments(batch)
    batch.add_argument("--params", required=True)
    batch.set_defaults(handler=_command_batch)

    reslice = subparsers.add_parser("reslice")
    _add_analysis_arguments(reslice)
    reslice.add_argument("--old-block-samples", type=int, required=True)
    reslice.add_argument("--new-block-samples", type=int, required=True)
    reslice.add_argument("--old-oversample", type=int, default=4)
    reslice.add_argument("--new-oversample", type=int, default=4)
    reslice.set_defaults(handler=_command_reslice)

    publish = subparsers.add_parser("publish")
    publish.add_argument("--report", required=True)
    publish.add_argument("--versions-root", required=True)
    publish.set_defaults(handler=_command_publish)

    query = subparsers.add_parser("query")
    query.add_argument("--versions-root", required=True)
    query.add_argument("--version")
    query.add_argument("--output")
    query.set_defaults(handler=_command_query)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = args.handler(args)
        _emit(result)
        return 0
    except DiagnosticError as exc:
        sys.stderr.buffer.write(canonical_bytes(exc.as_dict()))
        sys.stderr.buffer.flush()
        return _EXIT_CODES.get(exc.code, 1)
    except (TypeError, ValueError, OSError) as exc:
        error = DiagnosticError(E_CONFIG_INVALID, str(exc))
        sys.stderr.buffer.write(canonical_bytes(error.as_dict()))
        sys.stderr.buffer.flush()
        return _EXIT_CODES[E_CONFIG_INVALID]


if __name__ == "__main__":
    raise SystemExit(main())

