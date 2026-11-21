#!/usr/bin/env python3
"""Scripted experiment entry: python3 run_experiment.py [root] [seed]."""

import sys

from fpkit.cli import main

if __name__ == "__main__":
    root = sys.argv[1] if len(sys.argv) > 1 else "workdir/experiment"
    seed = int(sys.argv[2]) if len(sys.argv) > 2 else 20261122
    sys.exit(main(["experiment", "--root", root, "--seed", str(seed)]))
