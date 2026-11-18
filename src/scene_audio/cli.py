from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any

from .errors import BusinessError, ValidationFailure
from .generator import generate_demo
from .jsonio import write_json_atomic
from .render import render_scene


def _error_payload(error: BusinessError) -> dict[str, Any]:
    payload = error.to_dict()
    payload["ok"] = False
    return payload


def _write_error(out_dir: Path, error: BusinessError) -> None:
    try:
        out_dir.mkdir(parents=True, exist_ok=True)
        write_json_atomic(out_dir / "error.json", _error_payload(error))
    except OSError:
        pass


def _render_command(args: argparse.Namespace) -> int:
    out_dir = Path(args.out)
    stop_requested = False

    def handle_sigint(_signum: int, _frame: Any) -> None:
        nonlocal stop_requested
        stop_requested = True

    previous_handler = signal.getsignal(signal.SIGINT)
    signal.signal(signal.SIGINT, handle_sigint)
    try:
        result = render_scene(
            args.scene,
            out_dir,
            snapshot_path=args.snapshot,
            fresh=args.fresh,
            verify_reference=args.verify_reference,
            interrupt_requested=lambda: stop_requested,
        )
        summary = {
            "ok": result.exit_code == 0,
            "interrupted": result.interrupted,
            "exit_code": result.exit_code,
            "report": result.report_path.name if result.report_path else None,
            "master": result.master_path.name if result.master_path else None,
        }
        if result.error is not None:
            summary["error"] = result.error
        print(json.dumps(summary, sort_keys=True, ensure_ascii=False))
        return int(result.exit_code)
    except ValidationFailure as exc:
        _write_error(out_dir, exc)
        print(
            json.dumps(_error_payload(exc), sort_keys=True, ensure_ascii=False),
            file=sys.stderr,
        )
        return int(exc.exit_code)
    except BusinessError as exc:
        _write_error(out_dir, exc)
        print(
            json.dumps(_error_payload(exc), sort_keys=True, ensure_ascii=False),
            file=sys.stderr,
        )
        return int(exc.exit_code)
    finally:
        signal.signal(signal.SIGINT, previous_handler)


def _generate_command(args: argparse.Namespace) -> int:
    out_dir = Path(args.out)
    scene = generate_demo(out_dir)
    print(
        json.dumps(
            {
                "ok": True,
                "scene": str(Path("scene.json")),
                "name": scene["name"],
                "duration": scene["duration"],
                "tracks": len(scene["tracks"]),
            },
            sort_keys=True,
            ensure_ascii=False,
        )
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="scene-audio",
        description="Deterministic offline scene-audio renderer",
    )
    parser.add_argument("--version", action="version", version="0.1.0")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate", help="generate deterministic WAV assets and scene JSON"
    )
    generate.add_argument("--out", required=True, help="output asset directory")
    generate.set_defaults(handler=_generate_command)

    render = subparsers.add_parser(
        "render", help="render a scene to a stereo master and report"
    )
    render.add_argument("--scene", required=True, help="scene JSON path")
    render.add_argument("--out", required=True, help="deliverable directory")
    render.add_argument(
        "--snapshot", default=None, help="optional snapshot file path"
    )
    render.add_argument(
        "--fresh", action="store_true", help="discard any existing snapshot"
    )
    render.add_argument(
        "--verify-reference",
        default=None,
        help="compare final master bytes against a reference WAV",
    )
    render.set_defaults(handler=_render_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except ValidationFailure as exc:
        print(
            json.dumps(_error_payload(exc), sort_keys=True, ensure_ascii=False),
            file=sys.stderr,
        )
        return int(exc.exit_code)
    except BusinessError as exc:
        print(
            json.dumps(_error_payload(exc), sort_keys=True, ensure_ascii=False),
            file=sys.stderr,
        )
        return int(exc.exit_code)
    except OSError as exc:
        print(
            json.dumps(
                {"ok": False, "code": "E_IO", "message": str(exc)},
                sort_keys=True,
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

