from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .engine import master_project
from .errors import MasteringError


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m mastering",
        description="Deterministic offline multitrack mastering",
    )
    parser.add_argument("--manifest", required=True, help="project manifest JSON")
    parser.add_argument("--automation", required=True, help="automation curves JSON")
    parser.add_argument("--config", required=True, help="output configuration JSON")
    parser.add_argument("--output", required=True, help="master WAV path")
    parser.add_argument("--report", help="processing report JSON path")
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="print the complete report on success",
    )
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        report = master_project(
            args.manifest,
            args.automation,
            args.config,
            args.output,
            args.report,
        )
    except MasteringError as exc:
        print(
            json.dumps(exc.as_dict(), ensure_ascii=False, sort_keys=True),
            file=sys.stdout,
        )
        return exc.exit_code
    except Exception as exc:
        payload = {
            "success": False,
            "error": {
                "code": "INTERNAL_ERROR",
                "message": str(exc),
                "issues": [],
            },
        }
        print(json.dumps(payload, ensure_ascii=False, sort_keys=True), file=sys.stdout)
        return 1

    if args.verbose:
        print(json.dumps(report, ensure_ascii=False, sort_keys=True))
    else:
        summary = {
            "success": True,
            "output": str(Path(args.output).resolve()),
            "report": str(Path(args.report).resolve()) if args.report else None,
            "frames": report["project"]["output_frames"],
            "sample_rate": report["project"]["sample_rate"],
            "channels": report["project"]["channels"],
            "format": report["project"]["format"],
            "sha256": report["wav"]["sha256"],
            "true_peak_dbtp": report["limiter"]["output_true_peak_dbtp"],
        }
        print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

