from __future__ import annotations

import argparse
import os

import uvicorn

from rec_gateway.config import GatewayConfig
from rec_gateway.main import create_app


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the recommendation inference gateway")
    parser.add_argument("--config", default=None)
    parser.add_argument("--host", default=None)
    parser.add_argument("--port", type=int, default=None)
    parser.add_argument("--model-dir", default=None)
    parser.add_argument("--database", default=None)
    parser.add_argument("--log-level", default="info")
    args = parser.parse_args()

    config = GatewayConfig.load(args.config)
    if args.host is not None:
        config.host = args.host
    if args.port is not None:
        config.port = args.port
    if args.model_dir is not None:
        config.model_dir = config.model_dir.__class__(args.model_dir).resolve()
    if args.database is not None:
        config.database = config.database.__class__(args.database).resolve()

    os.environ.setdefault("OMP_NUM_THREADS", "1")
    os.environ.setdefault("ORT_NUM_THREADS", "1")
    app = create_app(config=config)
    uvicorn.run(
        app,
        host=config.host,
        port=config.port,
        log_level=args.log_level,
        http="h11",
        loop="asyncio",
        workers=1,
        limit_concurrency=max(4, config.max_concurrency * 2),
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
