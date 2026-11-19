from __future__ import annotations

import json
import socket
import tempfile
from pathlib import Path
from typing import Any

from poweraudit.config import AppConfig
from poweraudit.inventory import Inventory, load_inventory, load_scenario
from poweraudit.simulator import build_agent_server, build_simulated_devices

ROOT = Path(__file__).resolve().parents[1]


def reserve_udp_ports(count: int) -> list[int]:
    sockets: list[socket.socket] = []
    ports: list[int] = []
    try:
        for _ in range(count):
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sock.bind(("127.0.0.1", 0))
            sockets.append(sock)
            ports.append(sock.getsockname()[1])
    finally:
        for sock in sockets:
            sock.close()
    return ports


class DemoEnvironment:
    def __init__(
        self,
        *,
        disabled: set[str] | None = None,
        total_timeout_seconds: float = 2.0,
        device_timeout_seconds: float = 0.6,
        request_timeout_seconds: float = 0.12,
    ) -> None:
        self.temp_dir = tempfile.TemporaryDirectory(prefix="poweraudit-test-")
        self.base = Path(self.temp_dir.name)
        inventory_raw = json.loads(
            (ROOT / "data" / "asset_inventory.json").read_text(encoding="utf-8")
        )
        scenario_raw = json.loads(
            (ROOT / "data" / "simulator_scenario.json").read_text(encoding="utf-8")
        )
        ports = reserve_udp_ports(len(inventory_raw["devices"]))
        for device, port in zip(inventory_raw["devices"], ports):
            device["port"] = port
        inventory_path = self.base / "inventory.json"
        scenario_path = self.base / "scenario.json"
        inventory_path.write_text(
            json.dumps(inventory_raw, ensure_ascii=False), encoding="utf-8"
        )
        scenario_path.write_text(
            json.dumps(scenario_raw, ensure_ascii=False), encoding="utf-8"
        )
        config_raw = {
            "inventory_path": str(inventory_path),
            "scenario_path": str(scenario_path),
            "database": {"path": str(self.base / "audit.db")},
            "http": {"host": "127.0.0.1", "port": 0},
            "snmp": {
                "request_timeout_seconds": request_timeout_seconds,
                "device_timeout_seconds": device_timeout_seconds,
                "total_timeout_seconds": total_timeout_seconds,
                "retries": 0,
                "max_workers": 8,
                "max_varbinds_per_request": 24,
            },
            "emergency": {
                "battery_threshold_percent": 25,
                "runtime_threshold_minutes": 20,
                "failover_reserve_percent": 10,
                "min_transfer_load_percent": 1.0,
            },
        }
        config_path = self.base / "config.json"
        config_path.write_text(
            json.dumps(config_raw, ensure_ascii=False), encoding="utf-8"
        )
        self.config = AppConfig.load(config_path)
        self.inventory: Inventory = load_inventory(inventory_path)
        self.scenario = load_scenario(scenario_path)
        enabled = set(self.inventory.device_order)
        if disabled:
            enabled -= disabled
        self.simulator = build_agent_server(
            build_simulated_devices(
                self.inventory,
                self.scenario,
                enabled_device_ids=enabled,
            ),
            backend="auto",
        )

    def start(self) -> "DemoEnvironment":
        self.simulator.start_thread()
        return self

    def close(self) -> None:
        self.simulator.stop()
        self.temp_dir.cleanup()

    def __enter__(self) -> "DemoEnvironment":
        return self.start()

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()
