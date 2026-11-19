from __future__ import annotations

import json
import logging
import signal
import threading
import time
from typing import Any

from .audit import AuditStore
from .config import Config, load_config
from .db import DatabaseSet
from .http_api import ApiContext, LocalHttpServer, stable_json
from .observability import Observability
from .schema import ensure_schema, reset_data, seed_rows
from .sync import SyncEngine
from .trace import TraceService
from .validation import ValidationManager

LOGGER = logging.getLogger(__name__)


class Runtime:
    def __init__(self, config: Config):
        self.config = config
        self.audit = AuditStore(config.audit.sqlite_path)
        self.databases = DatabaseSet(
            config.sites,
            config.limits.max_connections,
            config.limits.statement_timeout_ms,
        )
        self.sync = SyncEngine(config, self.databases, self.audit)
        self.validation = ValidationManager(config, self.databases, self.audit)
        self.observability = Observability(
            config, self.databases, self.audit, self.sync, self.validation
        )
        self.trace = TraceService(config, self.databases, self.audit)
        self.http: LocalHttpServer | None = None

    def bootstrap_schema(self) -> None:
        for site in self.config.ordered_sites:
            with self.databases.pool(site.name).connection(
                self.config.limits.sync_acquire_timeout_ms
            ) as conn:
                ensure_schema(conn, site.name, site.priority)

    def seed(self, members: int, orders: int, reset: bool = False) -> None:
        if reset:
            self.audit.reset_all()
        self.bootstrap_schema()
        for site in self.config.ordered_sites:
            with self.databases.pool(site.name).connection(
                self.config.limits.sync_acquire_timeout_ms
            ) as conn:
                if reset:
                    reset_data(conn)
                seed_rows(conn, members, orders)

    def start(self) -> None:
        self.sync.start()
        self.validation.start_worker()
        self.observability.start()
        context = ApiContext(
            self.config,
            self.audit,
            self.sync,
            self.validation,
            self.observability,
            self.trace,
        )
        self.http = LocalHttpServer(self.config, context)
        self.http.start()

    def stop(self) -> None:
        if self.http:
            self.http.stop()
        self.sync.stop()
        self.validation.stop()
        self.observability.stop()
        self.databases.close()

    def serve_forever(self) -> None:
        assert self.http is not None
        self.http.serve_forever()


def configure_logging(level: str = "INFO") -> None:
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )


def run_serve(config: Config, bootstrap: bool = True) -> int:
    runtime = Runtime(config)
    if bootstrap:
        runtime.bootstrap_schema()
    runtime.start()
    stopping = threading.Event()

    def stop_handler(signum, frame):
        LOGGER.info("stopping on signal %s", signum)
        stopping.set()

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(sig, stop_handler)
        except (ValueError, OSError):
            pass
    LOGGER.info(
        "service listening on http://%s:%s",
        config.listen_host,
        config.listen_port,
    )
    try:
        while not stopping.wait(0.5):
            pass
    finally:
        runtime.stop()
    return 0


def wait_validation(
    audit: AuditStore, batch_id: str, timeout_seconds: float = 180.0
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        batch = audit.validation_batch(batch_id)
        if batch and batch["status"] == "completed":
            return batch
        time.sleep(0.1)
    raise TimeoutError(f"validation batch {batch_id} did not complete")


def run_cli(args) -> int:
    configure_logging(args.log_level)
    config = load_config(args.config)
    if args.command == "init":
        runtime = Runtime(config)
        try:
            runtime.seed(args.members, args.orders, reset=args.reset)
        finally:
            runtime.databases.close()
        print(
            json.dumps(
                {
                    "members": args.members,
                    "orders": args.orders,
                    "ok": True,
                    "reset": bool(args.reset),
                    "sites": sorted(config.sites),
                },
                sort_keys=True,
            )
        )
        return 0
    if args.command == "seed":
        runtime = Runtime(config)
        try:
            runtime.seed(args.members, args.orders, reset=args.reset)
        finally:
            runtime.databases.close()
        return 0
    if args.command == "serve":
        return run_serve(config, bootstrap=not args.no_bootstrap)
    if args.command == "validate":
        runtime = Runtime(config)
        try:
            runtime.bootstrap_schema()
            runtime.validation.start_worker()
            batch_id = runtime.validation.start(args.scope)
            if args.wait:
                batch = wait_validation(
                    runtime.audit, batch_id, timeout_seconds=args.timeout
                )
                mismatches = runtime.audit.validation_mismatches(
                    batch_id, args.limit
                )
                print(
                    stable_json(
                        {
                            "batch": batch,
                            "mismatches": mismatches,
                            "ok": True,
                        }
                    ).decode("utf-8")
                )
        finally:
            runtime.validation.stop()
            runtime.databases.close()
        return 0
    if args.command == "trace":
        runtime = Runtime(config)
        try:
            print(
                stable_json(
                    runtime.trace.trace(args.type, args.id, args.limit)
                ).decode("utf-8")
            )
        finally:
            runtime.databases.close()
        return 0
    if args.command == "conflicts":
        runtime = Runtime(config)
        try:
            rows = runtime.audit.query_conflicts(
                table_name=args.type,
                row_id=args.id,
                from_time=args.from_time,
                to_time=args.to_time,
                limit=args.limit,
            )
            print(stable_json({"conflicts": rows, "ok": True}).decode("utf-8"))
        finally:
            runtime.databases.close()
        return 0
    raise ValueError(f"unknown command {args.command}")

