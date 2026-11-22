"""Convenience launcher for Windows Explorer / PowerShell."""

from __future__ import annotations

import sys

from npr_workbench.cli import main


if __name__ == "__main__":
    raise SystemExit(main(["ui", *sys.argv[1:]]))

