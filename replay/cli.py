"""Command line interface for local deterministic replay."""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

from .baseline import compare_baseline, generate_baseline
from .compare import compare_paths
from .engine import VALID_MODES, ReplayEngine, run_recording
from .errors import ReplayError
from .generator import generate_samples
from .lock import MatchLock
from .model import serialization_order
from .snapshot import SnapshotStore
from .trajectory import write_trajectory
from .validation import atomic_write_json, load_recording


def _inject_list(values: list[str] | None) -> list[str]:
    if not values:
        return []
    result: list[str] = []
    for value in values:
        result.extend(item.strip() for item in value.split(",") if item.strip())
    return result


def _validate_mode(mode: str) -> str:
    if mode not in VALID_MODES:
        from .errors import E_MODE_INVALID, fail

        fail(
            E_MODE_INVALID,
            "mode must be 'fixed' or 'float'",
            "mode",
            details={"allowed": list(VALID_MODES), "found": mode},
        )
    return mode


def _write_json(path: str, payload: Any) -> None:
    atomic_write_json(path, payload)


def _snapshot_store(args: argparse.Namespace, recording: Any, mode: str) -> SnapshotStore | None:
    if not getattr(args, "snapshot_dir", None):
        return None
    return SnapshotStore(
        args.snapshot_dir,
        match_id=recording.match_id,
        mode=mode,
        config_digest=recording.config_digest(),
    )


def _with_lock(args: argparse.Namespace, recording: Any):
    lock_directory = getattr(args, "lock_dir", None) or os.path.join(
        os.getcwd(), ".replay-locks"
    )
    if getattr(args, "no_lock", False):
        class NullLock:
            def __enter__(self) -> None:
                return None

            def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
                return None

        return NullLock()
    return MatchLock(lock_directory, recording.match_id)


def _output_dir(args: argparse.Namespace) -> str:
    out_dir = os.path.abspath(args.out_dir)
    os.makedirs(out_dir, exist_ok=True)
    return out_dir


def cmd_generate(args: argparse.Namespace) -> dict[str, Any]:
    out_dir = _output_dir(args)
    manifest = generate_samples(out_dir, seed=args.seed)
    print(json.dumps({"ok": True, "out_dir": out_dir, **manifest}, sort_keys=True))
    return manifest


def cmd_run(args: argparse.Namespace) -> dict[str, Any]:
    out_dir = _output_dir(args)
    _validate_mode(args.mode)
    inject = _inject_list(args.inject)
    recording = load_recording(args.recording, inject=inject)
    store = _snapshot_store(args, recording, args.mode)
    with _with_lock(args, recording):
        run = run_recording(
            recording,
            args.mode,
            snapshot_store=store,
            cancel_after=args.cancel_after,
        )
        trajectory_path = os.path.join(out_dir, f"trajectory-{args.mode}.jsonl")
        trajectory_meta = write_trajectory(run, trajectory_path)
        summary = {
            **run.summary(),
            "trajectory": trajectory_meta,
            "serialization_order": serialization_order(),
            "derived_fields": recording.derived_fields,
            "injection": inject,
        }
        if args.baseline_out:
            baseline = generate_baseline(run)
            _write_json(args.baseline_out, baseline)
            summary["baseline_path"] = args.baseline_out
        summary_path = os.path.join(out_dir, f"summary-{args.mode}.json")
        _write_json(summary_path, summary)
        print(json.dumps(summary, sort_keys=True))
        return summary


def cmd_compare(args: argparse.Namespace) -> dict[str, Any]:
    out_dir = _output_dir(args)
    recording = load_recording(args.recording, inject=_inject_list(args.inject))
    with _with_lock(args, recording):
        fixed = run_recording(recording, "fixed")
        floating = run_recording(recording, "float")
        fixed_meta = write_trajectory(
            fixed, os.path.join(out_dir, "trajectory-fixed.jsonl")
        )
        float_meta = write_trajectory(
            floating, os.path.join(out_dir, "trajectory-float.jsonl")
        )
        report = compare_paths(recording, fixed_run=fixed, float_run=floating)
        report["trajectories"] = {"fixed": fixed_meta, "float": float_meta}
        report_path = os.path.join(out_dir, "dual-report.json")
        _write_json(report_path, report)
        report["report_path"] = report_path
        print(json.dumps(report, sort_keys=True))
        return report


def cmd_check_baseline(args: argparse.Namespace) -> dict[str, Any]:
    out_dir = _output_dir(args)
    _validate_mode(args.mode)
    recording = load_recording(args.recording, inject=_inject_list(args.inject))
    with _with_lock(args, recording):
        run = run_recording(recording, args.mode)
        report = compare_baseline(
            run,
            args.baseline,
            inject=_inject_list(args.inject_baseline),
        )
        report["recording"] = args.recording
        report["baseline"] = args.baseline
        report_path = os.path.join(out_dir, "baseline-report.json")
        _write_json(report_path, report)
        report["report_path"] = report_path
        print(json.dumps(report, sort_keys=True))
        return report


def cmd_recover(args: argparse.Namespace) -> dict[str, Any]:
    out_dir = _output_dir(args)
    _validate_mode(args.mode)
    recording = load_recording(args.recording, inject=_inject_list(args.inject))
    store = SnapshotStore(
        args.snapshot_dir,
        match_id=recording.match_id,
        mode=args.mode,
        config_digest=recording.config_digest(),
    )
    with _with_lock(args, recording):
        injection_context = None
        if args.truncate_latest:
            from .injection import truncate_latest_snapshot

            injection_context = truncate_latest_snapshot(args.snapshot_dir)
        try:
            snapshot = store.load_latest()
        except ReplayError as exc:
            raise exc.with_injection(injection_context) from None
        if snapshot is None:
            run = ReplayEngine(
                recording,
                args.mode,
                snapshot_store=store,
                cancel_after=args.cancel_after,
            ).run()
            recovered_from = 0
        else:
            run = ReplayEngine(
                recording,
                args.mode,
                snapshot_store=store,
                cancel_after=args.cancel_after,
            ).resume(snapshot)
            recovered_from = snapshot.step
        trajectory_path = os.path.join(
            out_dir, f"trajectory-recovered-{args.mode}.jsonl"
        )
        trajectory_meta = write_trajectory(run, trajectory_path)
        summary = {
            **run.summary(),
            "recovered_from": recovered_from,
            "trajectory": trajectory_meta,
            "injection": injection_context,
        }
        summary_path = os.path.join(out_dir, f"recovery-summary-{args.mode}.json")
        _write_json(summary_path, summary)
        print(json.dumps(summary, sort_keys=True))
        return summary


def cmd_verify(args: argparse.Namespace) -> dict[str, Any]:
    from .verify import verify_all

    out_dir = _output_dir(args)
    report = verify_all(out_dir, seed=args.seed)
    print(json.dumps(report["summary"], sort_keys=True))
    return report


def cmd_export(args: argparse.Namespace) -> dict[str, Any]:
    out_dir = _output_dir(args)
    _validate_mode(args.mode)
    recording = load_recording(args.recording, inject=_inject_list(args.inject))
    with _with_lock(args, recording):
        run = run_recording(recording, args.mode)
        from .versioning import downgrade_run

        exported = downgrade_run(run, target_version=args.target_version)
        output_path = args.output or os.path.join(
            out_dir, f"export-{args.target_version}.json"
        )
        _write_json(output_path, exported)
        result = {
            "ok": True,
            "target_version": args.target_version,
            "output": output_path,
            "steps": len(run.steps),
            "match_id": recording.match_id,
        }
        print(json.dumps(result, sort_keys=True))
        return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m replay")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out-dir", required=True)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.set_defaults(func=cmd_generate)

    run = sub.add_parser("run", help="advance one numeric path")
    run.add_argument("--recording", required=True)
    run.add_argument("--mode", required=True)
    run.add_argument("--out-dir", required=True)
    run.add_argument("--snapshot-dir")
    run.add_argument("--baseline-out")
    run.add_argument("--lock-dir")
    run.add_argument("--no-lock", action="store_true")
    run.add_argument("--cancel-after", type=int)
    run.add_argument("--inject", action="append")
    run.set_defaults(func=cmd_run)

    compare = sub.add_parser("compare", help="compare fixed and dual paths")
    compare.add_argument("--recording", required=True)
    compare.add_argument("--out-dir", required=True)
    compare.add_argument("--lock-dir")
    compare.add_argument("--no-lock", action="store_true")
    compare.add_argument("--inject", action="append")
    compare.set_defaults(func=cmd_compare)

    baseline = sub.add_parser("check-baseline", help="compare against a baseline")
    baseline.add_argument("--recording", required=True)
    baseline.add_argument("--baseline", required=True)
    baseline.add_argument("--mode", default="fixed")
    baseline.add_argument("--out-dir", required=True)
    baseline.add_argument("--lock-dir")
    baseline.add_argument("--no-lock", action="store_true")
    baseline.add_argument("--inject", action="append")
    baseline.add_argument("--inject-baseline", action="append")
    baseline.set_defaults(func=cmd_check_baseline)

    recover = sub.add_parser("recover", help="resume from the latest valid snapshot")
    recover.add_argument("--recording", required=True)
    recover.add_argument("--snapshot-dir", required=True)
    recover.add_argument("--mode", default="fixed")
    recover.add_argument("--out-dir", required=True)
    recover.add_argument("--lock-dir")
    recover.add_argument("--no-lock", action="store_true")
    recover.add_argument("--cancel-after", type=int)
    recover.add_argument("--truncate-latest", action="store_true")
    recover.add_argument("--inject", action="append")
    recover.set_defaults(func=cmd_recover)

    verify = sub.add_parser("verify", help="run the complete local verification suite")
    verify.add_argument("--out-dir", required=True)
    verify.add_argument("--seed", type=int, default=20261122)
    verify.set_defaults(func=cmd_verify)

    export = sub.add_parser("export", help="export a replay state document")
    export.add_argument("--recording", required=True)
    export.add_argument("--mode", default="fixed")
    export.add_argument("--target-version", choices=("v1", "v2"), default="v1")
    export.add_argument("--out-dir", required=True)
    export.add_argument("--output")
    export.add_argument("--lock-dir")
    export.add_argument("--no-lock", action="store_true")
    export.add_argument("--inject", action="append")
    export.set_defaults(func=cmd_export)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        args.func(args)
        return 0
    except ReplayError as exc:
        payload = exc.to_dict()
        print(json.dumps(payload, sort_keys=True), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print(
            json.dumps(
                {"ok": False, "code": "E_INTERRUPTED", "message": "interrupted"},
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
