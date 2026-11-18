"""Convenience wrapper for deterministic scene generation."""

import sys

from veilrender.cli import main


if __name__ == "__main__":
    raise SystemExit(main(["generate-scene", *sys.argv[1:]]))

