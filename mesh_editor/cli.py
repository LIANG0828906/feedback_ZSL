from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .acceptance import run_acceptance
from .editing import Selection, simplify_edge_collapse, transform_selection
from .errors import MeshEditorError
from .formats import parse_mesh
from .samplegen import generate_project_samples
from .versioning import VersionRepository


ROOT = Path(__file__).resolve().parents[1]


def _print(value: Any) -> None:
    print(json.dumps(value, sort_keys=True, indent=2, ensure_ascii=False))


def _open_repo(args: argparse.Namespace) -> VersionRepository:
    return VersionRepository(Path(args.workspace))


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mesh-editor",
        description="Versioned OBJ/STL surface mesh editor",
    )
    sub = parser.add_subparsers(dest="command")

    samples = sub.add_parser("generate-samples", help="generate deterministic local samples and baseline")
    samples.add_argument("--root", default=str(ROOT))
    samples.add_argument("--weld-tolerance", type=float, default=1e-6)

    info = sub.add_parser("info", help="show the current committed version")
    info.add_argument("--workspace", default=str(ROOT / "data/workspace"))

    recover = sub.add_parser("recover", help="verify the hash chain and recover committed state")
    recover.add_argument("--workspace", default=str(ROOT / "data/workspace"))

    export = sub.add_parser("export", help="export a committed version after hash validation")
    export.add_argument("--workspace", default=str(ROOT / "data/workspace"))
    export.add_argument("--version", type=int, required=True)
    export.add_argument("--format", choices=("obj", "stl"), required=True)
    export.add_argument("--format-version", type=int, choices=(1, 2), required=True)
    export.add_argument("--output", required=True)
    export.add_argument("--ascii-stl", action="store_true")
    export.add_argument("--allow-lossy", action="store_true")
    export.add_argument("--expected-hash")

    transform = sub.add_parser("transform", help="apply a selected face transform as a committed version")
    transform.add_argument("--workspace", default=str(ROOT / "data/workspace"))
    transform.add_argument("--faces", required=True, help="comma-separated face indices")
    transform.add_argument("--translate", nargs=3, type=float, default=(0.0, 0.0, 0.0))
    transform.add_argument("--rotate", nargs=3, type=float, default=(0.0, 0.0, 0.0))
    transform.add_argument("--scale", type=float, default=1.0)
    transform.add_argument("--weld-tolerance", type=float, default=1e-6)

    simplify = sub.add_parser("simplify", help="apply edge-collapse simplification as a committed version")
    simplify.add_argument("--workspace", default=str(ROOT / "data/workspace"))
    simplify.add_argument("--ratio", type=float, default=0.8)
    simplify.add_argument("--weld-tolerance", type=float, default=1e-6)

    rollback = sub.add_parser("rollback", help="move the committed head to an existing version")
    rollback.add_argument("--workspace", default=str(ROOT / "data/workspace"))
    rollback.add_argument("--version", type=int, required=True)

    undo = sub.add_parser("undo", help="move the committed head to the current version's parent")
    undo.add_argument("--workspace", default=str(ROOT / "data/workspace"))

    crash = sub.add_parser(
        "crash-after-stage",
        help="simulate forced termination after the snapshot is staged (intended for recovery tests)",
    )
    crash.add_argument("--workspace", default=str(ROOT / "data/workspace"))

    verify = sub.add_parser("verify", help="run the complete reproducible acceptance flow")
    verify.add_argument("--root", default=str(ROOT))

    gui = sub.add_parser("gui", help="launch the PyQt5/OpenGL desktop editor")
    gui.add_argument("--workspace", default=str(ROOT / "data/workspace"))
    gui.add_argument("--data", default=str(ROOT / "data"))
    gui.add_argument("--backend", choices=("auto", "opengl", "software"), default="auto")
    gui.add_argument("--smoke-test", action="store_true")

    return parser


def _faces(value: str) -> tuple[int, ...]:
    try:
        return tuple(sorted({int(part.strip()) for part in value.split(",") if part.strip()}))
    except ValueError as exc:
        raise ValueError("--faces must contain comma-separated integers") from exc


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    if args.command is None:
        from .ui import run_editor

        return run_editor(
            workspace=ROOT / "data/workspace",
            data_dir=ROOT / "data",
            backend="auto",
            smoke_test=False,
        )
    try:
        if args.command == "generate-samples":
            report = generate_project_samples(
                args.root, weld_tolerance=args.weld_tolerance
            )
            _print(report)
            return 0
        if args.command == "info":
            repo = _open_repo(args)
            mesh, entry = repo.load_current()
            _print(
                {
                    "version": entry["version"],
                    "content_hash": entry["content_hash"],
                    "entry_hash": entry["entry_hash"],
                    "next_version": repo.read_manifest()["next_version"],
                    "metrics": mesh.metrics(),
                    "manifest": str(repo.manifest_path),
                }
            )
            return 0
        if args.command == "recover":
            report = _open_repo(args).recover()
            _print(report.to_dict())
            return 0
        if args.command == "export":
            report = _open_repo(args).export_version(
                args.version,
                args.output,
                args.format,
                args.format_version,
                expected_hash=args.expected_hash,
                ascii_stl=args.ascii_stl,
                allow_lossy=args.allow_lossy,
            )
            _print(report.to_dict())
            return 0
        if args.command == "transform":
            repo = _open_repo(args)
            mesh, current = repo.load_current()
            result = transform_selection(
                mesh,
                Selection(face_indices=_faces(args.faces)),
                translation=args.translate,
                rotation_degrees=args.rotate,
                scale=args.scale,
            )
            entry = repo.commit_edit(
                current["version"],
                result.mesh,
                metrics_before=result.metrics_before,
                metrics_after=result.metrics_after,
                operation=result.operation,
                weld_tolerance=args.weld_tolerance,
            )
            _print(entry)
            return 0
        if args.command == "simplify":
            repo = _open_repo(args)
            mesh, current = repo.load_current()
            result = simplify_edge_collapse(mesh, target_ratio=args.ratio)
            entry = repo.commit_edit(
                current["version"],
                result.mesh,
                metrics_before=result.metrics_before,
                metrics_after=result.metrics_after,
                operation=result.operation,
                weld_tolerance=args.weld_tolerance,
            )
            _print(entry)
            return 0
        if args.command == "rollback":
            _print(_open_repo(args).rollback_to(args.version))
            return 0
        if args.command == "undo":
            _print(_open_repo(args).undo_last_commit())
            return 0
        if args.command == "crash-after-stage":
            repo = _open_repo(args)
            mesh, current = repo.load_current()
            result = transform_selection(
                mesh, Selection(face_indices=(0,)), translation=(1.0, 0.0, 0.0)
            )
            os.environ["MESH_EDITOR_CRASH_AFTER_STAGE"] = "1"
            repo.commit_edit(
                current["version"],
                result.mesh,
                metrics_before=result.metrics_before,
                metrics_after=result.metrics_after,
                operation=result.operation,
                weld_tolerance=1e-6,
            )
            return 0
        if args.command == "verify":
            report = run_acceptance(args.root)
            _print(report)
            return 0 if report["all_pass"] else 1
        if args.command == "gui":
            from .ui import run_editor

            return run_editor(
                workspace=Path(args.workspace),
                data_dir=Path(args.data),
                backend=args.backend,
                smoke_test=args.smoke_test,
            )
    except MeshEditorError as exc:
        _print(exc.to_dict())
        return 2
    except Exception as exc:
        _print({"code": "E_INTERNAL", "message": f"{type(exc).__name__}: {exc}"})
        return 3
    return 1
