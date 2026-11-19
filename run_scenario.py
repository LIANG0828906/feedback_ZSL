"""Generate and execute a full deterministic scenario, then write report.json."""

from release_orchestrator.cli import main

if __name__ == "__main__":
    raise SystemExit(main(["scenario", *__import__("sys").argv[1:]]))
