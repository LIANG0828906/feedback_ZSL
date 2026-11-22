"""Standalone replay verification command."""

from __future__ import annotations

import argparse
import json

from .codes import E_OK
from .verifier import verify_replay


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Independently verify a TickArena replay")
    parser.add_argument("--replay", required=True)
    parser.add_argument("--events", required=True)
    parser.add_argument(
        "--include-final-state",
        action="store_true",
        help="include the full recomputed final state in output",
    )
    args = parser.parse_args(argv)
    report = verify_replay(args.replay, args.events)
    output = {
        "code": report.code,
        "message": report.message,
        "tick": report.tick,
        "rule": report.rule,
        "recorded": report.recorded,
        "recomputed": report.recomputed,
    }
    if args.include_final_state:
        output["final_state"] = report.final_state
    print(json.dumps(output, ensure_ascii=False, sort_keys=True))
    return 0 if report.code == E_OK else 2


if __name__ == "__main__":
    raise SystemExit(main())

