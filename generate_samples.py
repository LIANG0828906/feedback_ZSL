"""Reproducible sample generator entry point."""

from release_orchestrator.cli import main

if __name__ == "__main__":
    raise SystemExit(main(["generate", *__import__("sys").argv[1:]]))
