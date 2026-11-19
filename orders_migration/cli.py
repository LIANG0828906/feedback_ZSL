from __future__ import annotations

import argparse
import json
import logging
from typing import Any

from .config import Config
from .errors import AppError
from .http_api import create_server
from .service import MigrationService


def _print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=False))


def _payload(value: str | None) -> dict[str, Any]:
    if not value:
        return {}
    try:
        parsed = json.loads(value)
    except json.JSONDecodeError as exc:
        raise AppError("E_VALIDATION", f"invalid JSON: {exc}", http_status=400) from exc
    if not isinstance(parsed, dict):
        raise AppError("E_VALIDATION", "JSON value must be an object", http_status=400)
    return parsed


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="orders-migration")
    parser.add_argument("--config", default="config.json")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("validate", help="validate the complete database topology")
    sub.add_parser("serve", help="start the HTTP service")
    sub.add_parser("health", help="print a topology/health snapshot")

    write = sub.add_parser("write", help="execute one dual write")
    write.add_argument("--json", required=True)
    write.add_argument("--idempotency-key")

    get = sub.add_parser("get", help="read one order through the selected route")
    get.add_argument("--order-id", type=int, required=True)
    get.add_argument("--tenant-id", type=int, required=True)

    migrate = sub.add_parser("migrate", help="migrate or resume one tenant")
    migrate.add_argument("--tenant-id", type=int, required=True)
    migrate.add_argument("--max-batches", type=int)

    migrate_status = sub.add_parser("migration-status")
    migrate_status.add_argument("--tenant-id", type=int, required=True)

    check = sub.add_parser("check", help="reconcile legacy and shard rows")
    check.add_argument("--order-id-start", type=int, default=0)
    check.add_argument("--order-id-end", type=int, default=9_223_372_036_854_775_807)
    check.add_argument("--tenant-id", type=int)
    check.add_argument("--force", action="store_true")

    check_resume = sub.add_parser("check-resume")
    check_resume.add_argument("--run-id", required=True)
    check_status = sub.add_parser("check-status")
    check_status.add_argument("--run-id", required=True)

    rollback = sub.add_parser("rollback", help="drain writes and converge both sides")
    rollback.add_argument("--tenants", help="comma-separated tenant ids; default all")
    rollback.add_argument("--run-id")
    rollback_status = sub.add_parser("rollback-status")
    rollback_status.add_argument("--run-id", required=True)

    trace = sub.add_parser("trace")
    trace.add_argument("--order-id", type=int, required=True)

    fault = sub.add_parser("inject-fault")
    fault.add_argument("--stage", required=True)
    fault.add_argument("--times", type=int, default=1)
    return parser


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        config = Config.load(args.config)
        service = MigrationService.create(config)
    except AppError as exc:
        _print(exc.as_dict())
        return 3 if exc.code == "E_TOPOLOGY" else 2
    except Exception as exc:
        _print(
            {
                "ok": False,
                "errorCode": "E_STARTUP",
                "message": str(exc),
            }
        )
        return 2

    try:
        if args.command == "validate":
            _print(service.topology_report())
            return 0
        if args.command == "health":
            _print(service.health())
            return 0
        if args.command == "serve":
            service.start_recovery_threads()
            server = create_server(config, service)
            _print(
                {
                    "ok": True,
                    "event": "listening",
                    "url": f"http://{config.host}:{config.port}",
                    "schemaSignature": service.topology.schema_signature,
                }
            )
            try:
                server.serve_forever(poll_interval=0.25)
            except KeyboardInterrupt:
                pass
            finally:
                server.server_close()
            return 0
        if args.command == "write":
            _print(
                service.write_order(
                    _payload(args.json), args.idempotency_key
                )
            )
            return 0
        if args.command == "get":
            _print(service.get_order(args.order_id, args.tenant_id))
            return 0
        if args.command == "migrate":
            _print(
                service.migrate_tenant(args.tenant_id, args.max_batches)
            )
            return 0
        if args.command == "migration-status":
            _print(service.migration_status(args.tenant_id))
            return 0
        if args.command == "check":
            _print(
                service.check(
                    order_id_start=args.order_id_start,
                    order_id_end=args.order_id_end,
                    tenant_id=args.tenant_id,
                    force=args.force,
                )
            )
            return 0
        if args.command == "check-resume":
            _print(service.check(run_id=args.run_id))
            return 0
        if args.command == "check-status":
            _print(service.check_status(args.run_id))
            return 0
        if args.command == "rollback":
            tenants = (
                [int(value) for value in args.tenants.split(",") if value.strip()]
                if args.tenants
                else None
            )
            _print(service.rollback(tenants=tenants, run_id=args.run_id))
            return 0
        if args.command == "rollback-status":
            _print(service.rollback_status(args.run_id))
            return 0
        if args.command == "trace":
            _print({"ok": True, **service.trace(args.order_id)})
            return 0
        if args.command == "inject-fault":
            if not config.fault_injection_enabled:
                raise AppError(
                    "E_FORBIDDEN", "fault injection is disabled", http_status=403
                )
            _print(service.set_fault(args.stage, args.times))
            return 0
        parser.error("unknown command")
        return 2
    except AppError as exc:
        service.record_error(exc.code)
        _print(exc.as_dict())
        return 2
    finally:
        service.close()


if __name__ == "__main__":
    raise SystemExit(main())
