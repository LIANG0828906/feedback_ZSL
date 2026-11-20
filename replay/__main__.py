"""支持 ``python -m replay`` 启动 GUI。"""

import argparse

from .gui import run_gui


def main() -> int:
    parser = argparse.ArgumentParser(description="启动回放器图形界面")
    parser.add_argument("--samples", default="samples/matches")
    parser.add_argument("--baselines", default="samples/baselines")
    parser.add_argument("--output", default="output/gui")
    parser.add_argument("--config", default="config/replay.json")
    args = parser.parse_args()
    return run_gui(
        args.samples, args.baselines, args.output, args.config
    )


if __name__ == "__main__":
    raise SystemExit(main())
