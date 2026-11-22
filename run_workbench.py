#!/usr/bin/env python3
"""Convenience launcher for the PySide6 workbench."""

import sys

from formation_workbench.__main__ import main


if __name__ == "__main__":
    arguments = sys.argv[1:] or ["gui"]
    raise SystemExit(main(arguments))
