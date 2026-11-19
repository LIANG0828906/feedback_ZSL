"""Command-line entry points for the simulator and service."""

from __future__ import annotations

import argparse
import json
import logging
import signal
import sys
import threading
from typing import Any

from .config import AppConfig, ConfigError
from .database import Database
from .inventory import InventoryError, load_inventory, load_scenario
from .service import PowerAuditService, create_http_server
from .simulator import (
    SnmpAgentServer,
    build_agent_server,
    build_simulated_devices,
)


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _load(config_path: str) -> tuple[AppConfig, Any]:
    config = AppConfig.load(config_path)
    inventory = load_inventory(config.inventory_path)
    return config, inventory


def _simulator(
    config: AppConfig,
    inventory: Any,
    disabled: set[str] | None = None,
) -> Any:
    scenario = {}
    if config.scenario_path.exists():
        scenario = load_scenario(config.scenario_path)
    devices = build_simulated_devices(
        inventory,
        scenario,
        enabled_device_ids=None if not disabled else set(inventory.device_order) - disabled,
    )
    return build_agent_server(devices, backend=config.simulator_backend)


def cmd_simulate(args: argparse.Namespace) -> int:
    config, inventory = _load(args.config)
    server = _simulator(config, inventory, set(args.disable or []))
    stop = threading.Event()

    def handle_signal(_signum: int, _frame: Any) -> None:
        stop.set()

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)
    try:
        server.start_thread()
        print(
            f"SNMP simulator ready on {len(server.agents)} UDP endpoints",
            flush=True,
        )
        while not stop.wait(0.25):
            pass
    finally:
        server.stop()
        print("SNMP simulator stopped", flush=True)
    return 0


def cmd_serve(args: argparse.Namespace) -> int:
    config, inventory = _load(args.config)
    service = PowerAuditService(config, inventory, Database(config.database_path))
    simulator_server: SnmpAgentServer | None = None
    http_server = None
    stop = threading.Event()

    def handle_signal(_signum: int, _frame: Any) -> None:
        stop.set()

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)
    try:
        if args.with_simulator:
            simulator_server = _simulator(config, inventory)
            simulator_server.start_thread()
        http_server = create_http_server(service, port=args.port)
        http_server.timeout = 0.5
        host, port = http_server.server_address[:2]
        print(f"Power audit service listening on http://{host}:{port}", flush=True)
        while not stop.is_set():
            http_server.handle_request()
    finally:
        if http_server is not None:
            http_server.server_close()
        if simulator_server is not None:
            simulator_server.stop()
        print("Power audit service stopped", flush=True)
    return 0


def cmd_audit(args: argparse.Namespace) -> int:
    config, inventory = _load(args.config)
    service = PowerAuditService(config, inventory, Database(config.database_path))
    _print_json(service.run_audit(args.batch_id))
    return 0


def cmd_plan(args: argparse.Namespace) -> int:
    config, inventory = _load(args.config)
    service = PowerAuditService(config, inventory, Database(config.database_path))
    _print_json(
        service.create_maintenance_plan(
            {
                "batch_id": args.batch_id,
                "target_type": args.target_type,
                "target_id": args.target_id,
                "acceptable_server_ids": args.acceptable or [],
            }
        )
    )
    return 0


def cmd_emergency(args: argparse.Namespace) -> int:
    config, inventory = _load(args.config)
    service = PowerAuditService(config, inventory, Database(config.database_path))
    _print_json(
        service.create_emergency_plan(
            {"batch_id": args.batch_id, "ups_ids": args.ups or []}
        )
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="poweraudit")
    parser.add_argument("--log-level", default="INFO")
    subparsers = parser.add_subparsers(dest="command", required=True)

    simulate = subparsers.add_parser("simulate", help="run local SNMP agents")
    simulate.add_argument("--config", required=True)
    simulate.add_argument(
        "--disable",
        action="append",
        default=[],
        help="device ID to leave unbound (repeatable)",
    )
    simulate.set_defaults(func=cmd_simulate)

    serve = subparsers.add_parser("serve", help="run HTTP audit service")
    serve.add_argument("--config", required=True)
    serve.add_argument("--port", type=int)
    serve.add_argument(
        "--with-simulator",
        action="store_true",
        help="also start the local SNMP agents in this process",
    )
    serve.set_defaults(func=cmd_serve)

    audit = subparsers.add_parser("audit", help="run one audit and persist it")
    audit.add_argument("--config", required=True)
    audit.add_argument("--batch-id")
    audit.set_defaults(func=cmd_audit)

    plan = subparsers.add_parser("plan", help="create a maintenance plan")
    plan.add_argument("--config", required=True)
    plan.add_argument("--batch-id", required=True)
    plan.add_argument("--target-type", choices=("ups", "pdu"), required=True)
    plan.add_argument("--target-id", required=True)
    plan.add_argument("--acceptable", action="append")
    plan.set_defaults(func=cmd_plan)

    emergency = subparsers.add_parser(
        "emergency", help="create a low-battery emergency plan"
    )
    emergency.add_argument("--config", required=True)
    emergency.add_argument("--batch-id", required=True)
    emergency.add_argument("--ups", action="append")
    emergency.set_defaults(func=cmd_emergency)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    logging.basicConfig(
        level=getattr(logging, str(args.log_level).upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    try:
        return int(args.func(args))
    except (ConfigError, InventoryError) as exc:
        print(f"configuration error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
