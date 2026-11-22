#!/usr/bin/env python3
"""Run the OCR pipeline over the local sample dataset.

Supports pause/cancel via --max-intervals (simulates an interrupted run),
resume on restart, parallel recognition workers and multi-version export.
"""
import argparse
import json
import sys
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from ocr_pipeline import engine
from ocr_pipeline.pipeline import Pipeline, PipelineConfig


class _CountdownCancel:
    """Cancel event that trips after N committed intervals (pause simulation)."""

    def __init__(self, limit):
        self._event = threading.Event()
        self._limit = limit
        self._seen = 0
        self._lock = threading.Lock()

    def is_set(self):
        with self._lock:
            if self._seen >= self._limit:
                self._event.set()
            self._seen += 1
        return self._event.is_set()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", default="data")
    parser.add_argument("--source-version", default="3.0")
    parser.add_argument("--merge-threshold", type=float,
                        default=engine.DEFAULT_MERGE_THRESHOLD)
    parser.add_argument("--conf-threshold", type=float,
                        default=engine.DEFAULT_CONF_THRESHOLD)
    parser.add_argument("--lines-per-interval", type=int, default=2)
    parser.add_argument("--workers", type=int, default=1)
    parser.add_argument("--buffer-limit", type=int, default=1)
    parser.add_argument("--max-intervals", type=int, default=None,
                        help="stop after N interval commits (pause simulation)")
    parser.add_argument("--export", default=None,
                        help="target schema version to export after the run")
    args = parser.parse_args()

    config = PipelineConfig(
        data_dir=Path(args.data),
        source_version=args.source_version,
        merge_threshold=args.merge_threshold,
        conf_threshold=args.conf_threshold,
        lines_per_interval=args.lines_per_interval,
        max_workers=args.workers,
        buffer_limit=args.buffer_limit,
    )
    pipeline = Pipeline(config)
    cancel = _CountdownCancel(args.max_intervals) if args.max_intervals else None
    summary = pipeline.run(cancel_event=cancel)
    print(json.dumps(summary, indent=2, sort_keys=True))
    if args.export and summary["completed"]:
        reports = pipeline.export(args.export)
        n_warnings = sum(len(w) for w in reports.values())
        print(f"exported {len(reports)} results as v{args.export} "
              f"with {n_warnings} E_LOSSY warnings")
    elif args.export:
        print("run incomplete (paused/cancelled); export skipped — "
              "no complete export is claimed after interruption")
    return 0 if summary["completed"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
