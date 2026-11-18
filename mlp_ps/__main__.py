from __future__ import annotations

import sys
from typing import Optional, Sequence


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)
    if not args or args[0] in {"-h", "--help"}:
        print(
            "usage: python -m mlp_ps {server|worker|control|compare|demo|checkpoint} ...\n"
            "See README.md for complete Windows commands."
        )
        return 0
    command, rest = args[0], args[1:]
    if command == "server":
        from .server import main as entry

        return entry(rest)
    if command == "worker":
        from .worker import main as entry

        return entry(rest)
    if command == "control":
        from .control import main as entry

        return entry(rest)
    if command == "compare":
        from .compare import main as entry

        return entry(rest)
    if command == "demo":
        from .demo import main as entry

        return entry(rest)
    if command == "checkpoint":
        from .checkpoint import main as entry

        return entry(rest)
    print(f"unknown command: {command}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
