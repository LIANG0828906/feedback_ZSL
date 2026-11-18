#!/usr/bin/env python3
"""Thin wrapper so the renderer can run as: python elytron_render.py ..."""

from elytron.cli import main


if __name__ == "__main__":
    raise SystemExit(main())
