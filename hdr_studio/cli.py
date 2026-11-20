from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .demo import run_demo
from .generator import generate_sample_set, load_sample_manifest
from .session import ConversionSession


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="hdr-studio",
        description="Deterministic HDR/SDR conversion workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--workspace", required=True)
    generate.add_argument("--seed", type=int, default=20261121)

    demo = subparsers.add_parser(
        "demo", help="run the scripted deterministic conversion/recovery demo"
    )
    demo.add_argument("--workspace", required=True)
    demo.add_argument("--seed", type=int, default=20261121)
    demo.add_argument("--force", action="store_true")

    workbench = subparsers.add_parser("workbench", help="start the PySide6 desktop workbench")
    workbench.add_argument("--workspace", required=True)
    workbench.add_argument("--sample", help="sample file or sample name")
    workbench.add_argument("--offscreen", action="store_true")

    recover = subparsers.add_parser("recover", help="validate and restore the latest snapshot")
    recover.add_argument("--workspace", required=True)
    recover.add_argument("--sample", help="optional source sequence path")
    return parser


def _resolve_sample(workspace: Path, sample: str | None) -> Path | None:
    if sample is None:
        return None
    path = Path(sample)
    if path.exists():
        return path
    candidate = workspace / "samples" / sample
    if candidate.exists():
        return candidate
    candidate = workspace / "samples" / f"{sample}.hdrseq"
    if candidate.exists():
        return candidate
    raise FileNotFoundError(f"sample not found: {sample}")


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "generate":
        manifest = generate_sample_set(args.workspace, seed=args.seed)
        print(json.dumps(manifest, ensure_ascii=False, sort_keys=True, indent=2))
        return 0
    if args.command == "demo":
        baseline = run_demo(args.workspace, seed=args.seed, force=args.force)
        print(json.dumps(baseline, ensure_ascii=False, sort_keys=True, indent=2))
        return 0
    if args.command == "recover":
        workspace = Path(args.workspace)
        sample = _resolve_sample(workspace, args.sample)
        session = ConversionSession(workspace, sample)
        print(
            json.dumps(
                {
                    "latest_version": session.latest_record.version if session.latest_record else 0,
                    "preview": session.preview_state.to_dict(),
                    "recovery": (
                        session.recovery_report.to_dict()
                        if session.recovery_report is not None
                        else None
                    ),
                },
                ensure_ascii=False,
                sort_keys=True,
                indent=2,
            )
        )
        session.close()
        return 0
    if args.command == "workbench":
        if args.offscreen:
            import os

            os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
        from .gui import run_workbench

        workspace = Path(args.workspace)
        sample = _resolve_sample(workspace, args.sample)
        if sample is None:
            manifest = load_sample_manifest(workspace)
            if manifest and manifest.get("samples"):
                sample = workspace / "samples" / manifest["samples"][0]["file"]
        return run_workbench(workspace, sample)
    return 2


if __name__ == "__main__":
    sys.exit(main())

