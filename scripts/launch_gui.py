#!/usr/bin/env python3
"""Launch the PySide6 review desktop."""

from __future__ import annotations

import argparse

from cover_replay.gui import launch


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--sample")
    args = parser.parse_args()
    return launch(args.sample)


if __name__ == "__main__":
    raise SystemExit(main())
