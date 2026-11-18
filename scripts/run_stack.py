#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from inference_router.stack import LocalStack  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description="Run all replicas and the inference router")
    parser.add_argument("--config", default=str(ROOT / "config" / "router.json"))
    parser.add_argument("--fresh-db", action="store_true")
    args = parser.parse_args()
    stack = LocalStack(ROOT, args.config)
    stopped = threading.Event()
    try:
        stack.start_all_workers()
        stack.start_router(fresh_db=args.fresh_db)
        print(
            json.dumps(
                {
                    "ok": True,
                    "event": "stack_started",
                    "workers": stack.process_table(),
                    "grpc": stack.config.service.router_grpc,
                    "http": stack.config.service.http,
                },
                sort_keys=True,
            ),
            flush=True,
        )

        def stop(_signum, _frame):
            stopped.set()

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        stopped.wait()
        return 0
    finally:
        stack.stop_all()


if __name__ == "__main__":
    raise SystemExit(main())
