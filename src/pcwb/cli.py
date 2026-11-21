"""Command-line entry points for samples, demos, repositories, and rendering."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .async_edit import AsyncEditController
from .cache import OperationCache
from .demo import run_demo
from .diff import diff_clouds
from .errors import BusinessError
from .format import read_point_cloud
from .repository import PointCloudRepository
from .samplegen import generate_samples
from .selection import select_sphere
from .workbench import WorkbenchOptions, run_workbench


def _json_print(value: Any) -> None:
    print(json.dumps(value, indent=2, sort_keys=True))


def _error(exc: BusinessError) -> int:
    print(json.dumps(exc.to_dict(), indent=2, sort_keys=True), file=sys.stderr)
    return 2


def _cmd_samples(args: argparse.Namespace) -> int:
    _json_print(generate_samples(args.out, seed=args.seed))
    return 0


def _cmd_demo(args: argparse.Namespace) -> int:
    _json_print(
        run_demo(
            args.out,
            seed=args.seed,
            progress_threshold_ms=args.threshold_ms,
        )
    )
    return 0


def _cmd_inspect(args: argparse.Namespace) -> int:
    try:
        cloud = read_point_cloud(args.file)
    except BusinessError as exc:
        return _error(exc)
    _json_print(
        {
            "file": str(args.file),
            "count": cloud.count,
            "bounds": cloud.bounds.to_dict(),
            "sha256": cloud.content_hash,
        }
    )
    return 0


def _repo_for_gui(args: argparse.Namespace) -> PointCloudRepository:
    repo_path = Path(args.repo)
    manifest = repo_path / "manifest.json"
    if manifest.exists():
        return PointCloudRepository(repo_path)
    if not args.sample:
        raise BusinessError(
            "repository is not initialized; pass --sample to import a PCWB file"
        )
    cloud = read_point_cloud(args.sample)
    return PointCloudRepository.create(
        repo_path, cloud, source_name=Path(args.sample).name
    )


def _cmd_gui(args: argparse.Namespace) -> int:
    try:
        repo = _repo_for_gui(args)
        report = run_workbench(
            repo,
            WorkbenchOptions(
                headless=args.headless,
                frames=args.frames,
                width=args.width,
                height=args.height,
                progress_threshold_ms=args.threshold_ms,
                metrics_path=args.metrics,
                auto_smooth=args.auto_smooth,
                scripted_input=args.scripted_input,
            ),
        )
    except BusinessError as exc:
        return _error(exc)
    _json_print(report)
    return 0


def _cmd_versions(args: argparse.Namespace) -> int:
    try:
        repo = PointCloudRepository(args.repo)
        manifest = repo.manifest()
    except BusinessError as exc:
        return _error(exc)
    _json_print(
        {
            "current_version": manifest["current_version"],
            "max_version": manifest["max_version"],
            "versions": manifest["versions"],
        }
    )
    return 0


def _cmd_checkout(args: argparse.Namespace) -> int:
    try:
        repo = PointCloudRepository(args.repo)
        snapshot = repo.checkout(args.version)
    except BusinessError as exc:
        return _error(exc)
    _json_print(snapshot.identity())
    return 0


def _cmd_recover(args: argparse.Namespace) -> int:
    try:
        repo = PointCloudRepository(args.repo, recover=False)
        report = repo.recover()
        snapshot = repo.current_snapshot
    except BusinessError as exc:
        return _error(exc)
    _json_print(
        {
            "current_version": report.current_version,
            "removed_temporary_files": report.removed_temporary_files,
            "removed_orphan_objects": report.removed_orphan_objects,
            "current_hash": snapshot.content_hash,
            "point_count": snapshot.count,
        }
    )
    return 0


def _cmd_diff(args: argparse.Namespace) -> int:
    try:
        repo = PointCloudRepository(args.repo)
        old = repo.load_version(args.old)
        new = repo.load_version(args.new)
        result = diff_clouds(old.cloud, new.cloud)
    except BusinessError as exc:
        return _error(exc)
    _json_print(result.to_dict())
    return 0


def _cmd_edit(args: argparse.Namespace) -> int:
    """Non-interactive operator execution, useful for acceptance scripts."""

    try:
        repo = PointCloudRepository(args.repo)
        if args.sphere:
            center = args.center
            selection = select_sphere(repo.current_snapshot, center, args.radius)
        else:
            raise BusinessError("only --sphere selection is supported by this CLI")
        controller = AsyncEditController(
            repo,
            cache=OperationCache(),
            progress_threshold_ms=args.threshold_ms,
        )
        params = json.loads(args.params)
        controller.submit(args.operator, params, selection)
        if not controller.wait(args.timeout):
            controller.cancel()
            controller.wait(5.0)
            raise BusinessError("edit timed out", state=controller.status().state)
        status = controller.status()
        report = status.to_dict()
        if controller.last_commit is not None:
            report["commit"] = {
                "version": controller.last_commit.version,
                "output_hash": controller.last_commit.output_hash,
            }
        controller.close()
        if status.state != "succeeded":
            print(json.dumps(report, indent=2, sort_keys=True), file=sys.stderr)
            return 2
    except (BusinessError, ValueError) as exc:
        if isinstance(exc, BusinessError):
            return _error(exc)
        print(
            json.dumps({"code": "E_OP_INVALID", "message": str(exc)}), file=sys.stderr
        )
        return 2
    _json_print(report)
    return 0


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="pcwb", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    samples = sub.add_parser("samples", help="generate deterministic local samples")
    samples.add_argument("--out", default="artifacts/samples")
    samples.add_argument("--seed", type=int, default=20261122)
    samples.set_defaults(func=_cmd_samples)

    demo = sub.add_parser("demo", help="run scripted edits and write a trace")
    demo.add_argument("--out", default="artifacts/demo")
    demo.add_argument("--seed", type=int, default=20261122)
    demo.add_argument("--threshold-ms", type=float, default=50.0)
    demo.set_defaults(func=_cmd_demo)

    inspect = sub.add_parser("inspect", help="validate and describe a PCWB file")
    inspect.add_argument("file")
    inspect.set_defaults(func=_cmd_inspect)

    gui = sub.add_parser("gui", help="open the OpenGL workbench")
    gui.add_argument("--repo", required=True)
    gui.add_argument("--sample", help="import this sample if the repo is new")
    gui.add_argument("--headless", action="store_true")
    gui.add_argument("--frames", type=int, default=0)
    gui.add_argument("--width", type=int, default=1100)
    gui.add_argument("--height", type=int, default=760)
    gui.add_argument("--threshold-ms", type=float, default=250.0)
    gui.add_argument("--metrics")
    gui.add_argument(
        "--auto-smooth",
        action="store_true",
        help="submit one background smooth job (useful with --headless)",
    )
    gui.add_argument(
        "--scripted-input",
        action="store_true",
        help="inject a deterministic orbit event each headless frame",
    )
    gui.set_defaults(func=_cmd_gui)

    versions = sub.add_parser("versions", help="show repository versions")
    versions.add_argument("--repo", required=True)
    versions.set_defaults(func=_cmd_versions)

    checkout = sub.add_parser("checkout", help="set the active recovery version")
    checkout.add_argument("--repo", required=True)
    checkout.add_argument("version", type=int)
    checkout.set_defaults(func=_cmd_checkout)

    recover = sub.add_parser("recover", help="discard uncommitted/temporary files")
    recover.add_argument("--repo", required=True)
    recover.set_defaults(func=_cmd_recover)

    diff = sub.add_parser("diff", help="compare two committed versions")
    diff.add_argument("--repo", required=True)
    diff.add_argument("old", type=int)
    diff.add_argument("new", type=int)
    diff.set_defaults(func=_cmd_diff)

    edit = sub.add_parser("edit", help="run one background edit via sphere selection")
    edit.add_argument("--repo", required=True)
    edit.add_argument("--operator", required=True)
    edit.add_argument("--params", default="{}")
    edit.add_argument("--center", type=float, nargs=3, required=True)
    edit.add_argument("--radius", type=float, required=True)
    edit.add_argument("--sphere", action="store_true", default=True)
    edit.add_argument("--threshold-ms", type=float, default=50.0)
    edit.add_argument("--timeout", type=float, default=120.0)
    edit.set_defaults(func=_cmd_edit)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))
