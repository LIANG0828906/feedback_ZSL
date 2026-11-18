"""Command line entry points.

Fault injection (process kill, state-db lock, state-db corruption) is
performed by external processes; the ``lock-db`` / ``corrupt-db`` helpers
exist so those faults can be injected without touching engine code.
"""

from __future__ import annotations

import argparse
import json
import sqlite3
import sys
import time
from pathlib import Path

from .batchpath import baseline_windows
from .config import JobConfig
from .datagen import generate
from .engine import run_job
from .errors import EngineError


def _cmd_make_config(args) -> int:
    JobConfig().save(args.out)
    print(f"config written to {args.out}")
    return 0


def _cmd_generate(args) -> int:
    cfg = JobConfig.load(args.config)
    manifest = generate(cfg)
    print(json.dumps(manifest, indent=2, sort_keys=True))
    return 0


def _cmd_run(args) -> int:
    cfg = JobConfig.load(args.config)
    try:
        report = run_job(cfg, progress=lambda m: print(m, flush=True))
    except EngineError as exc:
        print(f"{exc.code}: {exc}", file=sys.stderr, flush=True)
        return exc.exit_code
    print("JOB_FINISHED", flush=True)
    print(
        f"windows={len(report['windows'])} "
        f"late={report['watermark']['late_events']} "
        f"dedup_dropped={report['input']['dedup_dropped']}",
        flush=True,
    )
    return 0


def _cmd_baseline(args) -> int:
    cfg = JobConfig.load(args.config)
    windows = baseline_windows(cfg)
    out = Path(args.out) if args.out else None
    payload = json.dumps(windows, indent=2, sort_keys=True) + "\n"
    if out:
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(payload, encoding="utf-8")
        print(f"baseline written to {out} ({len(windows)} windows)")
    else:
        print(payload, end="")
    return 0


def _cmd_lock_db(args) -> int:
    """Hold an exclusive lock on the state db (fault injection helper)."""
    path = Path(args.db)
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), timeout=1.0)
    conn.execute("PRAGMA locking_mode=EXCLUSIVE")
    conn.execute("BEGIN EXCLUSIVE")
    conn.execute("CREATE TABLE IF NOT EXISTS fault_hold(x INTEGER)")
    print(f"LOCKED {path}", flush=True)
    time.sleep(args.seconds)
    conn.rollback()
    conn.close()
    return 0


def _cmd_corrupt_db(args) -> int:
    """Overwrite the state db header (fault injection helper)."""
    path = Path(args.db)
    with path.open("r+b") as fh:
        fh.write(b"\xde\xad\xbe\xef" * 64)
    print(f"CORRUPTED {path}", flush=True)
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="risklake")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("make-config")
    p.add_argument("--out", required=True)
    p.set_defaults(func=_cmd_make_config)

    p = sub.add_parser("generate")
    p.add_argument("--config", required=True)
    p.set_defaults(func=_cmd_generate)

    p = sub.add_parser("run")
    p.add_argument("--config", required=True)
    p.set_defaults(func=_cmd_run)

    p = sub.add_parser("baseline")
    p.add_argument("--config", required=True)
    p.add_argument("--out", default=None)
    p.set_defaults(func=_cmd_baseline)

    p = sub.add_parser("lock-db")
    p.add_argument("--db", required=True)
    p.add_argument("--seconds", type=float, default=30.0)
    p.set_defaults(func=_cmd_lock_db)

    p = sub.add_parser("corrupt-db")
    p.add_argument("--db", required=True)
    p.set_defaults(func=_cmd_corrupt_db)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
