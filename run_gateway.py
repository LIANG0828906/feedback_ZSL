from __future__ import annotations

import argparse
import os
from pathlib import Path

import uvicorn

from gateway.app import create_app
from gateway.config import load_config


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the ranking canary gateway")
    parser.add_argument("--config", default="config.json", help="JSON configuration path")
    parser.add_argument("--host", default=None)
    parser.add_argument("--port", type=int, default=None)
    parser.add_argument("--reload", action="store_true")
    args = parser.parse_args()

    os.chdir(Path(__file__).resolve().parent)
    config = load_config(args.config)
    application = create_app(args.config)
    uvicorn.run(
        application,
        host=args.host or str(config.service.get("host", "127.0.0.1")),
        port=args.port or int(config.service.get("port", 8080)),
        log_level=str(config.service.get("log_level", "info")).lower(),
        access_log=True,
        timeout_keep_alive=5,
    )


if __name__ == "__main__":
    main()
