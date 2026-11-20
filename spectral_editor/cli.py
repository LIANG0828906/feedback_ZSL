from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .canonical import sha256_bytes
from .demo import run_demo, verify_determinism
from .errors import BusinessError, recovery_invalid
from .library import load_library
from .persistence import load_snapshot
from .samples import generate_samples


def _json_print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m spectral_editor",
        description="Deterministic spectral material color-management lab",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--seed", type=int, default=1907)
    generate.add_argument("--out", type=Path, default=Path("artifacts/samples"))

    demo = subparsers.add_parser("demo", help="run the scripted version-evolution demo")
    demo.add_argument("--seed", type=int, default=1907)
    demo.add_argument("--out", type=Path, default=Path("artifacts/demo"))

    verify = subparsers.add_parser("verify", help="run the demo twice and compare bytes")
    verify.add_argument("--seed", type=int, default=1907)
    verify.add_argument("--out", type=Path, default=Path("artifacts/verify"))

    gui = subparsers.add_parser("gui", help="open the Qt editor from a snapshot")
    gui.add_argument("--snapshot", type=Path, required=True)
    gui.add_argument("--screenshot", type=Path, default=None)
    gui.add_argument("--exit-after-ms", type=int, default=0)

    recover = subparsers.add_parser("recover", help="validate and optionally resume a snapshot")
    recover.add_argument("--snapshot", type=Path, required=True)
    recover.add_argument("--resume", action="store_true")

    validate = subparsers.add_parser("validate", help="validate a snapshot without previews")
    validate.add_argument("--snapshot", type=Path, required=True)

    return parser


def _cmd_generate(args: argparse.Namespace) -> int:
    generated = generate_samples(args.out, args.seed)
    snapshot_path = generated.out_dir / "state" / "snapshot.json"
    snapshot = load_snapshot(snapshot_path)
    _json_print(
        {
            "library_id": generated.library_id,
            "seed": args.seed,
            "materials": len(generated.definitions),
            "generation": snapshot.generation,
            "publication_hash": snapshot.publication_hash,
            "snapshot_hash": sha256_bytes(snapshot_path.read_bytes()),
            "out": str(generated.out_dir),
        }
    )
    return 0


def _cmd_demo(args: argparse.Namespace) -> int:
    summary = run_demo(args.seed, args.out)
    _json_print(summary)
    return 0


def _cmd_verify(args: argparse.Namespace) -> int:
    result = verify_determinism(args.seed, args.out)
    _json_print(result)
    return 0 if result["byte_identical"] else 1


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(
        args.snapshot,
        screenshot=args.screenshot,
        exit_after_ms=args.exit_after_ms,
    )


def _cmd_recover(args: argparse.Namespace) -> int:
    library, pending = load_library(args.snapshot)
    output: dict[str, Any] = {
        "library_id": library.current_snapshot.library_id,
        "generation": library.current_snapshot.generation,
        "publication_hash": library.current_snapshot.publication_hash,
        "pending_job": pending.to_dict() if pending is not None else None,
        "ui_state": dict(library.current_snapshot.ui_state),
        "resumed": False,
    }
    if args.resume and pending is not None:
        result = library.resume_pending(pending)
        output["resumed"] = True
        output["published_generation"] = result.generation
        output["resume_publication_hash"] = result.publication_hash
        output["recomputed_count"] = result.recomputed_count
        output["reused_count"] = result.reused_count
    _json_print(output)
    library.close()
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    snapshot = load_snapshot(args.snapshot)
    _json_print(
        {
            "valid": True,
            "library_id": snapshot.library_id,
            "generation": snapshot.generation,
            "publication_hash": snapshot.publication_hash,
            "materials": sorted(snapshot.histories),
            "ui_state": dict(snapshot.ui_state),
        }
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _cmd_generate(args)
        if args.command == "demo":
            return _cmd_demo(args)
        if args.command == "verify":
            return _cmd_verify(args)
        if args.command == "gui":
            return _cmd_gui(args)
        if args.command == "recover":
            return _cmd_recover(args)
        if args.command == "validate":
            return _cmd_validate(args)
        raise recovery_invalid(f"unknown command {args.command!r}")
    except BusinessError as exc:
        print(json.dumps(exc.as_dict(), ensure_ascii=False, sort_keys=True), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
