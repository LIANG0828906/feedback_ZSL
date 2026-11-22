#!/usr/bin/env python3
"""Run all deterministic acceptance checks and write JSON/Markdown evidence."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from audio_repair.verification import render_markdown_summary, run_acceptance


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", type=Path, default=Path("artifacts/samples"))
    parser.add_argument("--work", type=Path, default=Path("artifacts/verify"))
    parser.add_argument("--exports", type=Path, default=Path("artifacts/exports"))
    parser.add_argument(
        "--report",
        type=Path,
        default=Path("artifacts/acceptance_report.json"),
    )
    parser.add_argument(
        "--summary",
        type=Path,
        default=Path("artifacts/acceptance_summary.md"),
    )
    args = parser.parse_args()
    report = run_acceptance(args.samples, args.work, args.exports, report_path=args.report)
    args.summary.parent.mkdir(parents=True, exist_ok=True)
    args.summary.write_text(render_markdown_summary(report), encoding="utf-8")
    print(
        json.dumps(
            {
                "ok": report["ok"],
                "summary": report.get("summary"),
                "report": str(args.report),
                "summary_markdown": str(args.summary),
                "failure": report.get("failure"),
            },
            indent=2,
            ensure_ascii=False,
        )
    )
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

