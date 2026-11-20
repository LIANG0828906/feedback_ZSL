"""Compatibility wrapper for scripted local runs."""

from dustrender.cli import main

if __name__ == "__main__":
    raise SystemExit(main())
