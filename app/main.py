from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import sys
import threading

from .config import ConfigError, load_config
from .database import Database
from .dispatcher import DispatchManager
from .server import WebhookApiServer


LOGGER = logging.getLogger(__name__)


def _print_error(error: ConfigError) -> None:
    print(
        json.dumps(error.as_dict(), sort_keys=True, separators=(",", ":")),
        file=sys.stderr,
        flush=True,
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Tenant-aware webhook dispatcher")
    parser.add_argument(
        "--check-config",
        action="store_true",
        help="validate tenant configuration and exit",
    )
    args = parser.parse_args(argv)
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    try:
        tenants = load_config()
    except ConfigError as exc:
        _print_error(exc)
        return 2

    if args.check_config:
        print(
            json.dumps(
                {"status": "ok", "tenant_count": len(tenants)},
                sort_keys=True,
                separators=(",", ":"),
            )
        )
        return 0

    database = Database(
        os.environ.get("DB_PATH", "/data/webhook-lab.db")
    )
    database.initialize()
    database.sync_tenants(tenants)
    manager = DispatchManager(
        database,
        max_workers=int(os.environ.get("WORKER_COUNT", "64")),
        lease_ms=int(os.environ.get("LEASE_MS", "5000")),
        poll_seconds=float(os.environ.get("SCHEDULER_POLL_SECONDS", "0.02")),
        recovery_successes=int(os.environ.get("RECOVERY_SUCCESSES", "2")),
    )
    manager.start()

    address = os.environ.get("API_BIND", "0.0.0.0")
    port = int(os.environ.get("API_PORT", "8080"))
    server = WebhookApiServer((address, port), database)
    LOGGER.info(
        "dispatcher started address=%s:%s tenants=%s workers=%s db=%s",
        address,
        port,
        len(tenants),
        manager.max_workers,
        database.path,
    )

    def stop_server(signum: int, _frame: object) -> None:
        LOGGER.info("received signal %s, shutting down", signum)
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop_server)
    signal.signal(signal.SIGINT, stop_server)
    try:
        server.serve_forever(poll_interval=0.25)
    finally:
        manager.stop()
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

