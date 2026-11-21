from __future__ import annotations

import sys


def main() -> int:
    if len(sys.argv) > 1:
        from .cli import main as cli_main

        return cli_main(sys.argv[1:])
    try:
        from .gui import main as gui_main
    except ImportError as exc:
        print(
            "PySide6 is required for the desktop UI. "
            "Use `python -m motion_workbench.cli --help` for headless commands. "
            f"Import error: {exc}",
            file=sys.stderr,
        )
        return 1
    return gui_main([])


if __name__ == "__main__":
    raise SystemExit(main())
