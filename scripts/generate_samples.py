from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from sync_kernel.samples import (
    DEFAULT_COMMIT_WINDOW,
    DEFAULT_MATCH_ID,
    DEFAULT_SEED,
    DEFAULT_TICKS,
    write_samples,
)


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic local match samples.")
    parser.add_argument("--output", type=Path, default=Path("samples"))
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--match-id", default=DEFAULT_MATCH_ID)
    parser.add_argument("--ticks", type=int, default=DEFAULT_TICKS)
    parser.add_argument("--commit-window", type=int, default=DEFAULT_COMMIT_WINDOW)
    args = parser.parse_args()
    bundle, states = write_samples(
        args.output,
        seed=args.seed,
        match_id=args.match_id,
        ticks=args.ticks,
        commit_window=args.commit_window,
    )
    print(
        json.dumps(
            {
                "output": str(args.output),
                "match_id": bundle.context.match_id,
                "seed": bundle.context.seed,
                "ticks": args.ticks,
                "units": sorted(bundle.initial_state.units),
                "input_fingerprint": bundle.context.input_fingerprint,
                "final_hash": states[-1].snapshot_hash,
            },
            indent=2,
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

