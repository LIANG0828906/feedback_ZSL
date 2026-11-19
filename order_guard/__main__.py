from __future__ import annotations

import argparse
import logging
import sys

import uvicorn

from .app import create_app
from .config import ConfigError, load_config


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the local order guard service")
    parser.add_argument(
        "--config",
        default="config/service.json",
        help="path to the JSON service configuration",
    )
    parser.add_argument("--host", help="override configured bind host")
    parser.add_argument("--port", type=int, help="override configured bind port")
    parser.add_argument(
        "--log-level",
        default="info",
        choices=["critical", "error", "warning", "info", "debug"],
    )
    args = parser.parse_args()
    logging.basicConfig(
        level=getattr(logging, args.log_level.upper()),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    try:
        config = load_config(args.config)
    except ConfigError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    app = create_app(config)
    uvicorn.run(
        app,
        host=args.host or config.service.host,
        port=args.port or config.service.port,
        log_level=args.log_level,
        access_log=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
