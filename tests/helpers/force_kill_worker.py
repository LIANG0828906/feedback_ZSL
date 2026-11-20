"""Subprocess used to prove recovery after an abrupt process kill."""

from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from game_audio_restorer.recipes import load_recipe
from game_audio_restorer.restore import RestorationEngine


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--wav", required=True)
    parser.add_argument("--recipe", required=True)
    parser.add_argument("--work", required=True)
    parser.add_argument("--marker", required=True)
    args = parser.parse_args()
    marker = Path(args.marker)

    def after_commit(_entry: dict) -> None:
        marker.write_text("committed\n", encoding="utf-8")
        time.sleep(60)

    engine = RestorationEngine(args.wav, load_recipe(args.recipe), args.work)
    engine.run(on_segment=after_commit)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
