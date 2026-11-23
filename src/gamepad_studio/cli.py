"""Command-line entry points for replay, validation, snapshots, and self-check."""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .compare import compare_results
from .errors import DivergenceError, StudioError
from .models import CurveSpec
from .pipeline import Pipeline
from .replay import check_truth, replay_full
from .samples import generate_samples
from .selfcheck import run_selfcheck
from .service import StudioSession
from .session import HotSwitchSession, plan_hot_switch
from .snapshot import SnapshotStore
from .validation import load_sample, write_sample_json


def _print(payload: Any) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, default=str))


def _sample(args: argparse.Namespace) -> Any:
    return load_sample(Path(args.samples_root) / args.sample)


def command_generate(args: argparse.Namespace) -> int:
    _print(generate_samples(args.out))
    return 0


def command_validate(args: argparse.Namespace) -> int:
    bundle = load_sample(Path(args.sample))
    truth = check_truth(bundle, replay_full(bundle))
    _print(
        {
            "code": "OK",
            "sample": bundle.metadata(),
            "truth_passed": truth.passed,
            "truth_checks": len(truth.checks),
            "truth_failures": [failure.to_dict() for failure in truth.failures],
        }
    )
    return 0 if truth.passed else 1


def command_replay(args: argparse.Namespace) -> int:
    bundle = load_sample(Path(args.sample))
    pipeline = Pipeline(bundle, max_event_buffer=args.event_buffer)
    run = pipeline.recompute()
    truth = check_truth(bundle, run.result)
    payload = {
        "sample": bundle.metadata(),
        "frames": len(run.result.frames),
        "result_hash": run.result.result_hash,
        "recomputed_nodes": run.recomputed_count,
        "reused_nodes": run.reused_count,
        "truth_pass": truth.passed,
    }
    if args.output:
        write_sample_json(args.output, run.result.to_dict())
        payload["output"] = str(Path(args.output).resolve())
    if args.details:
        payload["result"] = run.result.to_dict()
    _print(payload)
    return 0 if truth.passed else 1


def command_incremental(args: argparse.Namespace) -> int:
    bundle = load_sample(Path(args.sample))
    pipeline = Pipeline(bundle, max_event_buffer=args.event_buffer)
    initial = pipeline.recompute()
    device = bundle.device
    changed = None
    if args.axis and args.deadzone is not None:
        device = device.with_axis(args.axis, deadzone=args.deadzone)
        run = pipeline.update(device=device)
        changed = f"axis:{args.axis}.deadzone"
    elif args.rule and args.curve_exponent is not None:
        bindings = bundle.bindings.with_rule(
            args.rule,
            curve=CurveSpec(kind="power", exponent=args.curve_exponent),
        )
        run = pipeline.update(bindings=bindings)
        changed = f"rule:{args.rule}.curve"
    else:
        run = pipeline.recompute()
    full_bundle = replace(
        pipeline.bundle,
        device=pipeline.device,
        bindings=pipeline.bindings,
    )
    full = Pipeline(full_bundle, max_event_buffer=args.event_buffer).recompute()
    diff = compare_results(run.result, full.result, raise_on_divergence=True)
    _print(
        {
            "code": "OK",
            "changed": changed,
            "generation": run.generation,
            "recomputed": run.recomputed_count,
            "reused": run.reused_count,
            "invalidated": [
                entry.to_dict()
                for entry in run.invalidations
                if entry.invalidated
            ],
            "incremental_vs_full": diff.to_dict(),
            "initial_hash": initial.result.result_hash,
            "incremental_hash": run.result.result_hash,
            "full_hash": full.result.result_hash,
        }
    )
    return 0


def command_snapshot(args: argparse.Namespace) -> int:
    bundle = load_sample(Path(args.sample))
    pipeline = Pipeline(bundle, max_event_buffer=args.event_buffer)
    pipeline.recompute()
    store = SnapshotStore(args.path)
    if args.action == "write":
        report = store.write(pipeline)
        _print(report.to_dict())
        return 0
    load = store.load(pipeline)
    payload = load.to_dict()
    if load.valid:
        pipeline.install_cache_entries(load.entries)
        run = pipeline.recompute()
        payload["reused_after_load"] = run.reused_count
        payload["recomputed_after_load"] = run.recomputed_count
        payload["result_hash"] = run.result.result_hash
    _print(payload)
    return 0 if load.valid else 1


def command_switch(args: argparse.Namespace) -> int:
    source = load_sample(Path(args.source))
    target = load_sample(Path(args.target))
    session = HotSwitchSession(
        source,
        target,
        switch_t_ms=args.at,
        max_event_buffer=args.event_buffer,
    )
    analysis = session.result.analysis
    _print(
        {
            "code": "OK",
            **session.result.to_dict(),
            "direct_verification": session.verify_direct_target(),
            "analysis": analysis.to_dict(),
        }
    )
    return 0


def command_switch_conflict(args: argparse.Namespace) -> int:
    source = load_sample(Path(args.source))
    target = load_sample(Path(args.target))
    analysis = plan_hot_switch(
        source,
        target,
        target_bindings=source.bindings,
    )
    HotSwitchSession.raise_for_conflicts(analysis)
    _print({"code": "OK", "conflicts": 0})
    return 0


def command_selfcheck(args: argparse.Namespace) -> int:
    report = run_selfcheck(
        samples_root=args.samples,
        artifacts_root=args.artifacts,
        include_ui=not args.no_ui,
    )
    _print(
        {
            "summary": report["summary"],
            "report": str((Path(args.artifacts) / "validation_report.json").resolve()),
            "checks": [
                {"name": check["name"], "passed": check["passed"]}
                for check in report["checks"]
            ],
        }
    )
    return 0 if report["summary"]["passed"] else 1


def command_app(args: argparse.Namespace) -> int:
    from .app import main as app_main

    app_args = [
        "--samples",
        args.samples,
        "--snapshot-root",
        args.snapshot_root,
    ]
    if args.sample:
        app_args.extend(["--sample", args.sample])
    if args.smoke_test:
        app_args.extend(
            [
                "--smoke-test",
                "--screenshot",
                args.screenshot,
            ]
        )
    return app_main(app_args)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="padweave",
        description="PadWeave deterministic gamepad mapping studio",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out", default="samples")
    generate.set_defaults(function=command_generate)

    validate = subparsers.add_parser("validate", help="validate one sample and truth")
    validate.add_argument("--sample", required=True)
    validate.set_defaults(function=command_validate)

    replay = subparsers.add_parser("replay", help="full replay to action frames")
    replay.add_argument("--sample", required=True)
    replay.add_argument("--output")
    replay.add_argument("--details", action="store_true")
    replay.add_argument("--event-buffer", type=int, default=256)
    replay.set_defaults(function=command_replay)

    incremental = subparsers.add_parser(
        "incremental",
        help="change one parameter and verify incremental vs full replay",
    )
    incremental.add_argument("--sample", required=True)
    incremental.add_argument("--axis")
    incremental.add_argument("--deadzone", type=float)
    incremental.add_argument("--rule")
    incremental.add_argument("--curve-exponent", type=float)
    incremental.add_argument("--event-buffer", type=int, default=256)
    incremental.set_defaults(function=command_incremental)

    snapshot = subparsers.add_parser("snapshot", help="write or load a snapshot")
    snapshot.add_argument("--sample", required=True)
    snapshot.add_argument("--path", default="artifacts/snapshots/state.json")
    snapshot.add_argument("--action", choices=["write", "load"], default="write")
    snapshot.add_argument("--event-buffer", type=int, default=256)
    snapshot.set_defaults(function=command_snapshot)

    switch = subparsers.add_parser("switch", help="hot-switch to another layout")
    switch.add_argument("--source", required=True)
    switch.add_argument("--target", required=True)
    switch.add_argument("--at", type=int, default=2000)
    switch.add_argument("--event-buffer", type=int, default=256)
    switch.set_defaults(function=command_switch)

    switch_conflict = subparsers.add_parser(
        "switch-conflict",
        help="demonstrate E_MAP conflict reporting",
    )
    switch_conflict.add_argument("--source", required=True)
    switch_conflict.add_argument("--target", required=True)
    switch_conflict.set_defaults(function=command_switch_conflict)

    selfcheck = subparsers.add_parser("selfcheck", help="run all acceptance checks")
    selfcheck.add_argument("--samples", default="samples")
    selfcheck.add_argument("--artifacts", default="artifacts")
    selfcheck.add_argument("--no-ui", action="store_true")
    selfcheck.set_defaults(function=command_selfcheck)

    app = subparsers.add_parser("app", help="launch the PySide6 dashboard")
    app.add_argument("--samples", default="samples")
    app.add_argument("--sample")
    app.add_argument("--snapshot-root", default="artifacts/snapshots")
    app.add_argument("--smoke-test", action="store_true")
    app.add_argument("--screenshot", default="artifacts/ui_dashboard.png")
    app.set_defaults(function=command_app)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.function(args))
    except DivergenceError as exc:
        _print(exc.to_dict())
        return 3
    except StudioError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2), file=sys.stderr)
        return 2


__all__ = ["build_parser", "main"]


if __name__ == "__main__":
    raise SystemExit(main())
