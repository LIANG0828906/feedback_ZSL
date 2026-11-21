from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .errors import SurfaceViewerError
from .graph import DependencyGraph
from .model import load_assembly
from .runtime import SurfaceRuntime
from .samplegen import operation_changes, write_baseline, write_cases


def _print(value: Any) -> None:
    print(
        json.dumps(
            value,
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
            allow_nan=False,
        )
    )


def _load_operations(path: Path) -> list[dict[str, Any]]:
    return json.loads(path.read_text(encoding="utf-8"))["operations"]


def command_validate(args: argparse.Namespace) -> int:
    assembly = load_assembly(args.assembly)
    _print(
        {
            "valid": True,
            "assembly_id": assembly.assembly_id,
            "assembly_version": assembly.version_hash,
            "patch_count": len(assembly.patches),
            "patches": {
                patch.patch_id: patch.version_hash for patch in assembly.patches
            },
        }
    )
    return 0


def command_graph(args: argparse.Namespace) -> int:
    assembly = load_assembly(args.assembly)
    graph = DependencyGraph.build(assembly)
    graph.assert_publishable()
    _print(
        {
            "assembly_id": assembly.assembly_id,
            **graph.summary(None),
            "artifact_nodes": [node.node_id for node in graph.artifact_nodes()],
        }
    )
    return 0


def command_generate(args: argparse.Namespace) -> int:
    written = write_cases(args.output, seed=args.seed)
    _print({"generated": [str(path) for path in written]})
    return 0


def command_baseline(args: argparse.Namespace) -> int:
    summary = write_baseline(
        args.samples,
        args.output,
        preview_backend=args.preview_backend,
        verify_full=not args.no_verify,
    )
    _print(
        {
            "baseline": str(Path(args.output)),
            "cases": {
                name: {
                    "commit_seq": value["commit_seq"],
                    "trace_sha256": value["trace_sha256"],
                }
                for name, value in summary["cases"].items()
            },
        }
    )
    return 0


def command_run(args: argparse.Namespace) -> int:
    case_dir = Path(args.case)
    assembly = load_assembly(case_dir / "assembly.json")
    operations = _load_operations(case_dir / "operations.json")
    runtime = SurfaceRuntime.bootstrap(
        assembly,
        args.store,
        preview_backend=args.preview_backend,
        verify_full=not args.no_verify,
    )
    actions = [
        {
            "commit_seq": runtime.bootstrap_result.update.commit.commit_seq,
            "assembly_version": runtime.engine.assembly.version_hash,
            "screen_preview_hash": runtime.engine.screen_preview.content_hash,
        }
    ]
    for operation in operations:
        patch_id, changes = operation_changes(runtime.engine.assembly, operation)
        update = runtime.update(
            patch_id, changes, verify_full=not args.no_verify
        )
        actions.append(
            {
                "commit_seq": update.commit.commit_seq,
                "operation": operation,
                "assembly_version": update.result.assembly.version_hash,
                "affected_artifacts": list(
                    update.result.invalidations.affected_ids
                ),
                "reused_count": len(update.result.reused_ids),
                "recomputed_count": len(update.result.recomputed_ids),
                "screen_preview_hash": update.result.screen_preview.content_hash,
            }
        )
    _print(
        {
            "assembly_id": assembly.assembly_id,
            "store": str(Path(args.store)),
            "actions": actions,
        }
    )
    return 0


def command_recover(args: argparse.Namespace) -> int:
    case_dir = Path(args.case)
    assembly = load_assembly(case_dir / "assembly.json")
    runtime = SurfaceRuntime.bootstrap(
        assembly,
        args.store,
        preview_backend=args.preview_backend,
        verify_full=not args.no_verify,
    )
    boot = runtime.bootstrap_result
    _print(
        {
            "recovered": True,
            "previous_commit_seq": boot.recovery.commit_seq,
            "new_commit_seq": boot.update.commit.commit_seq,
            "reused_from_disk": boot.reused_from_disk,
            "recomputed_after_recovery": boot.recomputed_after_recovery,
            "assembly_version": runtime.engine.assembly.version_hash,
            "screen_preview_hash": runtime.engine.screen_preview.content_hash,
            "verification": boot.update.result.verification,
        }
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="surface-viewer",
        description="Deterministic incremental surface product pipeline",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    validate_parser = subparsers.add_parser("validate")
    validate_parser.add_argument("assembly")
    validate_parser.set_defaults(handler=command_validate)

    graph_parser = subparsers.add_parser("graph")
    graph_parser.add_argument("assembly")
    graph_parser.set_defaults(handler=command_graph)

    generate_parser = subparsers.add_parser("generate")
    generate_parser.add_argument("--output", default="samples")
    generate_parser.add_argument("--seed", type=int, default=0)
    generate_parser.set_defaults(handler=command_generate)

    baseline_parser = subparsers.add_parser("baseline")
    baseline_parser.add_argument("--samples", default="samples")
    baseline_parser.add_argument("--output", default="baseline")
    baseline_parser.add_argument(
        "--preview-backend", choices=("software", "opengl"), default="software"
    )
    baseline_parser.add_argument("--no-verify", action="store_true")
    baseline_parser.set_defaults(handler=command_baseline)

    run_parser = subparsers.add_parser("run")
    run_parser.add_argument("--case", required=True)
    run_parser.add_argument("--store", required=True)
    run_parser.add_argument(
        "--preview-backend", choices=("software", "opengl"), default="software"
    )
    run_parser.add_argument("--no-verify", action="store_true")
    run_parser.set_defaults(handler=command_run)

    recover_parser = subparsers.add_parser("recover")
    recover_parser.add_argument("--case", required=True)
    recover_parser.add_argument("--store", required=True)
    recover_parser.add_argument(
        "--preview-backend", choices=("software", "opengl"), default="software"
    )
    recover_parser.add_argument("--no-verify", action="store_true")
    recover_parser.set_defaults(handler=command_recover)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except SurfaceViewerError as exc:
        print(
            json.dumps(exc.to_dict(), ensure_ascii=False, sort_keys=True),
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
