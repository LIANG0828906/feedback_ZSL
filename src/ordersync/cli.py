from __future__ import annotations

import argparse
import json
import sys

from .config import load_config
from .db import pyodbc_connector
from .errors import AppError
from .runtime import build_application, ready_payload
from .topology import TopologyValidator


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="ordersync")
    subparsers = parser.add_subparsers(dest="command", required=True)
    serve = subparsers.add_parser("serve", help="validate topology and start API")
    serve.add_argument("--config", required=True)
    validate = subparsers.add_parser(
        "validate", help="validate configuration and primary/replica topology"
    )
    validate.add_argument("--config", required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        config = load_config(args.config)
        if args.command == "validate":
            topology = TopologyValidator(config, pyodbc_connector).validate()
            print(
                json.dumps(
                    {
                        "valid": True,
                        "replication_mode": topology.replication_mode,
                        "schema_digest": topology.schema_digest,
                        "primary_watermark": str(topology.primary_watermark),
                        "replica_watermark": str(topology.replica_watermark),
                    },
                    sort_keys=True,
                    separators=(",", ":"),
                ),
                flush=True,
            )
            return 0
        application = build_application(config, pyodbc_connector)
        application.start()
        print(ready_payload(config), flush=True)
        application.serve_forever()
        return 0
    except AppError as exc:
        print(
            json.dumps(
                exc.payload(),
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ),
            file=sys.stderr,
            flush=True,
        )
        return 2
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

