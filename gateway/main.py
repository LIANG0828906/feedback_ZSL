"""uvicorn entry point: python -m gateway.main --config config.json"""

from __future__ import annotations

import argparse

import uvicorn

from .app import create_app
from .config import load_config


def main() -> None:
    parser = argparse.ArgumentParser(description="Inference gateway")
    parser.add_argument("--config", default=None, help="Path to JSON config file")
    args = parser.parse_args()

    config = load_config(args.config)
    app = create_app(config)
    uvicorn.run(app, host=config.host, port=config.port, log_level="info")


if __name__ == "__main__":
    main()
