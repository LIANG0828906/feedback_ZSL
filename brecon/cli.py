from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .baseline import check_baseline
from .config import SampleConfig
from .engine import ReplayEngine
from .errors import BreconError, error_dict
from .generator import (
    Profile,
    default_profiles,
    generate_workspace,
)
from .lock import RoomBusyLock, room_lock_path
from .recovery import load_snapshot_file, recover_engine
from .verifier import verify_workspace


def _print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2))


def _manifest(workspace: Path) -> dict[str, Any]:
    path = workspace / "active.json"
    if not path.is_file():
        raise BreconError(
            "E_WORKSPACE_INVALID",
            f"workspace manifest not found: {path}",
            field_path="active.json",
        )
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def _sample_dir(workspace: Path, sample_name: str | None) -> Path:
    manifest = _manifest(workspace)
    target = sample_name or manifest.get("active_sample")
    if not target:
        raise BreconError(
            "E_WORKSPACE_INVALID",
            "no active sample is configured",
            field_path="active_sample",
        )
    for description in manifest.get("samples", []):
        if description["name"] == target:
            return workspace / description["directory"]
    raise BreconError(
        "E_SAMPLE_NOT_FOUND",
        f"sample is not present in workspace manifest: {target}",
        details={"sample": target},
    )


def _generate_command(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).resolve()
    if args.players is not None:
        profiles = [
            Profile(
                players=args.players,
                steps=args.steps,
                disorder=args.disorder,
                seed=args.seed,
                name=args.name,
            )
        ]
    else:
        profiles = default_profiles(args.seed)
        if args.profiles and args.profiles != "all":
            wanted = {value.strip() for value in args.profiles.split(",") if value.strip()}
            profiles = [profile for profile in profiles if profile.disorder in wanted]
            if not profiles:
                raise BreconError(
                    "E_INPUT_INVALID",
                    f"unknown profile selection: {args.profiles}",
                    field_path="--profiles",
                )
    report = generate_workspace(workspace, profiles, force=args.force)
    _print({"ok": True, "workspace": str(workspace), **report})
    return 0


def _run_command(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).resolve()
    sample_dir = _sample_dir(workspace, args.sample)
    config = SampleConfig.load(sample_dir)
    with RoomBusyLock(config.room_id, room_lock_path(sample_dir)):
        if args.from_snapshot:
            engine, snapshot_path = recover_engine(
                config, write_snapshots=not args.no_snapshot
            )
            recovered_from = str(snapshot_path)
        else:
            engine = ReplayEngine.from_sample_dir(
                config.sample_dir, write_snapshots=not args.no_snapshot
            )
            recovered_from = None
        engine.run()
        baseline_result = None
        if config.baseline_path.is_file():
            baseline_result = check_baseline(engine)
        outputs = engine.write_outputs(include_baseline=False)
    payload = {
        "ok": bool(baseline_result is None or baseline_result.get("ok")),
        "sample": config.sample_dir.name,
        "room_id": config.room_id,
        "steps": len(engine.trace_entries),
        "head_state_hash": engine.trace_entries[-1]["state_hash"],
        "head_chain_hash": engine.trace_entries[-1]["chain_hash"],
        "baseline_verified": (
            None if baseline_result is None else baseline_result.get("ok", False)
        ),
        "baseline": baseline_result,
        "recovered_from": recovered_from,
        "conservation": engine.conservation_report(),
        "ordering": engine.ordering_events,
        "outputs": outputs,
    }
    _print(payload)
    return 0 if payload["ok"] else 1


def _verify_command(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).resolve()
    report = verify_workspace(
        workspace,
        sample_names=args.sample,
        write=not args.no_write,
        recovery_step=args.recovery_step,
    )
    _print(report)
    return 0 if report["ok"] else 1


def _inspect_command(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).resolve()
    sample_dir = _sample_dir(workspace, args.sample)
    config = SampleConfig.load(sample_dir)
    engine = ReplayEngine.from_sample_dir(sample_dir).run()
    latency = engine.latency_payload()
    snapshot = load_snapshot_file(
        config.snapshots_path / "latest.json", config
    ) if (config.snapshots_path / "latest.json").is_file() else None
    _print(
        {
            "ok": True,
            "sample": config.sample_dir.name,
            "room_id": config.room_id,
            "players": list(config.players),
            "steps": len(engine.trace_entries),
            "head_state_hash": engine.trace_entries[-1]["state_hash"],
            "head_chain_hash": engine.trace_entries[-1]["chain_hash"],
            "boxes": engine.state["boxes"],
            "inventory": engine.state["inventory"],
            "holds": engine.state["holds"],
            "revocations": engine.state["revocations"],
            "latency": latency["distribution"],
            "compensation": latency["compensation"],
            "sync_observations": latency["sync_observations"],
            "latest_snapshot_step": None if snapshot is None else snapshot["step"],
            "conservation": engine.conservation_report(),
            "ordering": engine.ordering_events,
        }
    )
    return 0


def _gui_command(args: argparse.Namespace) -> int:
    try:
        from .gui import launch_gui
    except ImportError as exc:
        raise BreconError(
            "E_GUI_DEPENDENCY",
            f"PySide6 is required for the desktop interface: {exc}",
        ) from exc

    try:
        return launch_gui(
            workspace=Path(args.workspace).resolve(),
            sample=args.sample,
            offscreen=args.offscreen,
            auto_run=args.auto_run,
            screenshot=args.screenshot,
        )
    except ImportError as exc:
        raise BreconError(
            "E_GUI_DEPENDENCY",
            f"PySide6 is required for the desktop interface: {exc}",
        ) from exc


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="br-econ",
        description="Deterministic airdrop economy replay and reconciliation",
    )
    subcommands = parser.add_subparsers(dest="command", required=True)

    generate = subcommands.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--workspace", default="artifacts/generated")
    generate.add_argument(
        "--profiles",
        default="all",
        help="all or comma-separated low,medium,high",
    )
    generate.add_argument("--seed", type=int, default=20261121)
    generate.add_argument("--players", type=int)
    generate.add_argument("--steps", type=int, default=12)
    generate.add_argument(
        "--disorder", choices=("low", "medium", "high"), default="medium"
    )
    generate.add_argument("--name")
    generate.add_argument("--force", action="store_true")
    generate.set_defaults(handler=_generate_command)

    run = subcommands.add_parser("run", help="advance a sample replay")
    run.add_argument("--workspace", default="artifacts/demo")
    run.add_argument("--sample")
    run.add_argument("--from-snapshot", action="store_true")
    run.add_argument("--no-snapshot", action="store_true")
    run.set_defaults(handler=_run_command)

    recover = subcommands.add_parser(
        "recover", help="recover from the latest valid snapshot"
    )
    recover.add_argument("--workspace", default="artifacts/demo")
    recover.add_argument("--sample")
    recover.add_argument("--no-snapshot", action="store_true")
    recover.set_defaults(handler=_run_command, from_snapshot=True)

    verify = subcommands.add_parser("verify", help="verify all workspace samples")
    verify.add_argument("--workspace", default="artifacts/demo")
    verify.add_argument("--sample", action="append")
    verify.add_argument("--recovery-step", type=int)
    verify.add_argument("--no-write", action="store_true")
    verify.set_defaults(handler=_verify_command)

    inspect = subcommands.add_parser("inspect", help="inspect the final economy state")
    inspect.add_argument("--workspace", default="artifacts/demo")
    inspect.add_argument("--sample")
    inspect.set_defaults(handler=_inspect_command)

    gui = subcommands.add_parser("gui", help="launch the local PySide6 interface")
    gui.add_argument("--workspace", default="artifacts/demo")
    gui.add_argument("--sample")
    gui.add_argument("--offscreen", action="store_true")
    gui.add_argument("--auto-run", action="store_true")
    gui.add_argument("--screenshot")
    gui.set_defaults(handler=_gui_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except BreconError as exc:
        _print(error_dict(exc))
        return 1
    except KeyboardInterrupt:
        _print(
            {
                "ok": False,
                "error": {
                    "code": "E_INTERRUPTED",
                    "message": "replay interrupted",
                },
            }
        )
        return 130


if __name__ == "__main__":
    sys.exit(main())

