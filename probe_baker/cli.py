"""Command-line entry points for local generation, baking, recovery, and preview."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .bake import BakeEngine
from .canonical import TraceLog, write_json_atomic
from .errors import ErrorCode, ProbeError
from .jobs import BakeJobManager
from .publish import PublishedStore
from .samples import write_sample_scenes
from .scene import Scene, load_scene, parse_scene
from .snapshot import SnapshotStore
from .strategy import STRATEGY_AUTO, STRATEGY_DENSE, STRATEGY_SPARSE, BakeConfig, choose_strategy


def _config_from_args(args: argparse.Namespace) -> BakeConfig:
    return BakeConfig(
        spacing=args.spacing,
        sh_order=args.order,
        direction_count=args.directions,
        compensation_strength=args.compensation,
        strategy=args.strategy,
        chunk_size=args.chunk_size,
    )


def _print_payload(payload: Any) -> None:
    print(json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False))


def _bake_and_commit(
    scene: Scene,
    config: BakeConfig,
    *,
    previous: Any,
    snapshot_root: Path,
    version: int,
    trace: TraceLog,
) -> tuple[Any, Any]:
    engine = BakeEngine(trace=trace)
    outcome = engine.bake(scene, config, previous=previous)
    store = SnapshotStore(snapshot_root)
    store.commit_state(outcome.state, version=version)
    publisher = PublishedStore()
    snapshot = publisher.publish(outcome.state, version=version)
    return outcome, snapshot


def cmd_generate(args: argparse.Namespace) -> int:
    room, city = write_sample_scenes(args.out)
    _print_payload(
        {
            "generated": [str(room), str(city)],
            "deterministic": True,
            "external_resources": False,
        }
    )
    return 0


def cmd_bake(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    config = _config_from_args(args)
    trace = TraceLog(args.log)
    output = Path(args.out)
    output.mkdir(parents=True, exist_ok=True)
    snapshot_root = Path(args.snapshot_root) if args.snapshot_root else output / "snapshots"
    outcome, snapshot = _bake_and_commit(
        scene,
        config,
        previous=None,
        snapshot_root=snapshot_root,
        version=1,
        trace=trace,
    )
    payload = outcome.to_dict()
    payload["published_version"] = snapshot.version
    payload["snapshot_root"] = str(snapshot_root)
    write_json_atomic(output / "bake_summary.json", payload)
    _print_payload(payload)
    return 0


def cmd_compare(args: argparse.Namespace) -> int:
    from .comparison import compare_strategies

    scene = load_scene(args.scene)
    base = _config_from_args(args)
    result = compare_strategies(scene, base, engine=BakeEngine(trace=TraceLog(args.log)))
    _print_payload(result)
    return 0


def cmd_incremental(args: argparse.Namespace) -> int:
    from .acceptance import run_incremental_demo

    scene = load_scene(args.scene)
    result = run_incremental_demo(scene, Path(args.out), _config_from_args(args))
    _print_payload(result)
    return 0


def cmd_recover(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    config = _config_from_args(args)
    trace = TraceLog(args.log)
    store = SnapshotStore(args.snapshot_root)
    report = store.recover(args.snapshot_dir)
    trace.event("recovery_inspected", **report.to_dict())
    if report.published:
        _print_payload({"recovery": report.to_dict(), "resumed": False})
        return 0
    engine = BakeEngine(trace=trace)
    outcome = engine.resume_from_recovery(report, scene, config)
    recovered_root = Path(args.out) / "recovered_snapshots"
    SnapshotStore(recovered_root).commit_state(outcome.state, version=1)
    publisher = PublishedStore()
    snapshot = publisher.publish(outcome.state, version=1)
    payload = report.to_dict()
    payload["resumed"] = True
    payload["resampled_count"] = int(outcome.plan.resampled.size) if outcome.plan else 0
    payload["resampled_probe_ids"] = outcome.plan.resampled.tolist() if outcome.plan else []
    payload["published_version"] = snapshot.version
    payload["recovered_snapshot_root"] = str(recovered_root)
    _print_payload(payload)
    return 0


def cmd_snapshot_kill(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    config = _config_from_args(args)
    trace = TraceLog(args.log)
    outcome = BakeEngine(trace=trace).bake(scene, config)
    trace.event(
        "forced_termination_injected",
        after_chunks=args.after_chunks,
        snapshot_version=args.version,
        note="process will terminate before a complete manifest is committed",
    )
    SnapshotStore(args.snapshot_root).commit_state(
        outcome.state,
        version=args.version,
        forced_terminate_after_chunks=args.after_chunks,
    )
    return 0


def cmd_viewer(args: argparse.Namespace) -> int:
    from .viewer import OpenGLProbeViewer, run_headless_preview

    scene = load_scene(args.scene)
    config = _config_from_args(args)
    trace = TraceLog(args.log)
    publisher = PublishedStore()
    engine = BakeEngine(trace=trace)
    initial = engine.bake(scene, config)
    publisher.publish(initial.state, version=1)
    snapshot_root = Path(args.out) / "snapshots"
    SnapshotStore(snapshot_root).commit_state(initial.state, version=1)
    manager = BakeJobManager(
        scene.scene_id,
        engine=engine,
        publisher=publisher,
        trace=trace,
        snapshot_store=SnapshotStore(snapshot_root),
    )
    manager.seed_committed_state(
        initial.state,
        publisher.pin(version=1),
    )
    try:
        if args.headless:
            run_headless_preview(
                scene,
                config,
                publisher=publisher,
                job=manager,
                snapshot_store=manager.snapshot_store,
            )
            return 0
        viewer = OpenGLProbeViewer(
            scene,
            config,
            publisher=publisher,
            job=manager,
            initial_state=initial.state,
            snapshot_store=manager.snapshot_store,
        )
        viewer.run()
    finally:
        manager.shutdown()
    return 0


def cmd_query(args: argparse.Namespace) -> int:
    store = SnapshotStore(args.snapshot_root)
    report = store.recover(args.snapshot_dir)
    if report.snapshot is None:
        raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot is not complete")
    query = report.snapshot.query_at(
        tuple(args.position),
        version=args.version,
        expected_hash=args.expected_hash,
    )
    _print_payload(query.to_dict())
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="probe-baker")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="write deterministic local sample scenes")
    generate.add_argument("--out", default="samples")
    generate.set_defaults(handler=cmd_generate)

    def add_bake_arguments(command: argparse.ArgumentParser) -> None:
        command.add_argument("--scene", required=True)
        command.add_argument("--spacing", type=float, default=None)
        command.add_argument("--order", type=int, default=3)
        command.add_argument("--directions", type=int, default=96)
        command.add_argument("--compensation", type=float, default=0.35)
        command.add_argument(
            "--strategy",
            choices=(STRATEGY_AUTO, STRATEGY_DENSE, STRATEGY_SPARSE),
            default=STRATEGY_AUTO,
        )
        command.add_argument("--chunk-size", type=int, default=64)
        command.add_argument("--log", default="artifacts/trace.jsonl")
        command.add_argument("--out", default="artifacts/run")

    bake = subparsers.add_parser("bake", help="bake and publish one local scene")
    add_bake_arguments(bake)
    bake.add_argument("--snapshot-root", default=None)
    bake.set_defaults(handler=cmd_bake)

    compare = subparsers.add_parser("compare", help="compare dense and sparse reconstruction")
    add_bake_arguments(compare)
    compare.set_defaults(handler=cmd_compare)

    incremental = subparsers.add_parser(
        "incremental", help="run scripted local incremental rebake demo"
    )
    add_bake_arguments(incremental)
    incremental.set_defaults(handler=cmd_incremental)

    recover = subparsers.add_parser("recover", help="recover a forced-terminated snapshot")
    recover.add_argument("--scene", required=True)
    recover.add_argument("--snapshot-root", required=True)
    recover.add_argument("--snapshot-dir", default=None)
    recover.add_argument("--spacing", type=float, default=None)
    recover.add_argument("--order", type=int, default=3)
    recover.add_argument("--directions", type=int, default=96)
    recover.add_argument("--compensation", type=float, default=0.35)
    recover.add_argument(
        "--strategy",
        choices=(STRATEGY_AUTO, STRATEGY_DENSE, STRATEGY_SPARSE),
        default=STRATEGY_DENSE,
    )
    recover.add_argument("--chunk-size", type=int, default=4)
    recover.add_argument("--log", default="artifacts/trace.jsonl")
    recover.add_argument("--out", default="artifacts/recovery")
    recover.set_defaults(handler=cmd_recover)

    kill = subparsers.add_parser(
        "snapshot-kill",
        help="write a partial snapshot then terminate without a clean exit report",
    )
    add_bake_arguments(kill)
    kill.add_argument("--snapshot-root", required=True)
    kill.add_argument("--version", type=int, default=1)
    kill.add_argument("--after-chunks", type=int, required=True)
    kill.set_defaults(handler=cmd_snapshot_kill)

    viewer = subparsers.add_parser("viewer", help="OpenGL preview or headless smoke path")
    add_bake_arguments(viewer)
    viewer.add_argument("--headless", action="store_true")
    viewer.set_defaults(handler=cmd_viewer)

    query = subparsers.add_parser("query", help="query a complete snapshot")
    query.add_argument("--snapshot-root", required=True)
    query.add_argument("--snapshot-dir", default=None)
    query.add_argument("--version", type=int, default=None)
    query.add_argument("--expected-hash", default=None)
    query.add_argument("--position", type=float, nargs=3, required=True)
    query.set_defaults(handler=cmd_query)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except ProbeError as exc:
        sys.stderr.write(json.dumps(exc.to_dict(), ensure_ascii=False) + "\n")
        return 3 if exc.code == ErrorCode.BUSY else 2
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

