from pathlib import Path

from haptic.cli import run_demo

if __name__ == "__main__":
    summary = run_demo(Path("samples"), Path("artifacts/demo"))
    print(summary["signal_sha256"])
