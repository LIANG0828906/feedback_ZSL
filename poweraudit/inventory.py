"""Asset inventory and simulation scenario validation."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class InventoryError(ValueError):
    pass


@dataclass(frozen=True)
class Inventory:
    raw: dict[str, Any]
    devices: dict[str, dict[str, Any]]
    servers: dict[str, dict[str, Any]]
    server_order: tuple[str, ...]
    device_order: tuple[str, ...]

    @property
    def ups_ids(self) -> tuple[str, ...]:
        return tuple(
            device_id
            for device_id in self.device_order
            if self.devices[device_id]["device_type"] == "ups"
        )

    @property
    def pdu_ids(self) -> tuple[str, ...]:
        return tuple(
            device_id
            for device_id in self.device_order
            if self.devices[device_id]["device_type"] == "pdu"
        )


def _require_str(obj: dict[str, Any], key: str, where: str) -> str:
    value = obj.get(key)
    if not isinstance(value, str) or not value.strip():
        raise InventoryError(f"{where}.{key} must be a non-empty string")
    return value.strip()


def _require_int(obj: dict[str, Any], key: str, where: str, minimum: int = 0) -> int:
    value = obj.get(key)
    if not isinstance(value, int) or isinstance(value, bool) or value < minimum:
        raise InventoryError(f"{where}.{key} must be an integer >= {minimum}")
    return value


def load_inventory(path: str | Path) -> Inventory:
    source = Path(path)
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise InventoryError(f"inventory file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise InventoryError(f"invalid inventory JSON: {exc}") from exc
    if not isinstance(raw, dict):
        raise InventoryError("inventory root must be an object")
    device_list = raw.get("devices")
    server_list = raw.get("servers")
    if not isinstance(device_list, list) or not isinstance(server_list, list):
        raise InventoryError("inventory.devices and inventory.servers must be arrays")

    devices: dict[str, dict[str, Any]] = {}
    device_order: list[str] = []
    for index, item in enumerate(device_list):
        where = f"devices[{index}]"
        if not isinstance(item, dict):
            raise InventoryError(f"{where} must be an object")
        device_id = _require_str(item, "device_id", where)
        if device_id in devices:
            raise InventoryError(f"duplicate device_id: {device_id}")
        device_type = _require_str(item, "device_type", where).lower()
        if device_type not in ("ups", "pdu"):
            raise InventoryError(f"{where}.device_type must be ups or pdu")
        host = _require_str(item, "host", where)
        port = _require_int(item, "port", where, 1)
        if port > 65535:
            raise InventoryError(f"{where}.port must be <= 65535")
        community = item.get("community", "public")
        if not isinstance(community, str) or not community:
            raise InventoryError(f"{where}.community must be a non-empty string")
        normalized = dict(item)
        normalized.update(
            {
                "device_id": device_id,
                "device_type": device_type,
                "host": host,
                "port": port,
                "community": community,
                "enabled": bool(item.get("enabled", True)),
            }
        )
        if device_type == "pdu":
            normalized["port_count"] = _require_int(item, "port_count", where, 1)
        devices[device_id] = normalized
        device_order.append(device_id)

    if not any(device["device_type"] == "ups" for device in devices.values()):
        raise InventoryError("inventory must contain at least one UPS")
    if not any(device["device_type"] == "pdu" for device in devices.values()):
        raise InventoryError("inventory must contain at least one PDU")

    servers: dict[str, dict[str, Any]] = {}
    server_order: list[str] = []
    for index, item in enumerate(server_list):
        where = f"servers[{index}]"
        if not isinstance(item, dict):
            raise InventoryError(f"{where} must be an object")
        server_id = _require_str(item, "server_id", where)
        if server_id in servers:
            raise InventoryError(f"duplicate server_id: {server_id}")
        power_inputs = item.get("power_inputs")
        if not isinstance(power_inputs, list) or not (1 <= len(power_inputs) <= 2):
            raise InventoryError(f"{where}.power_inputs must contain one or two entries")
        normalized_inputs = []
        seen_paths: set[str] = set()
        for input_index, power_input in enumerate(power_inputs):
            input_where = f"{where}.power_inputs[{input_index}]"
            if not isinstance(power_input, dict):
                raise InventoryError(f"{input_where} must be an object")
            path_id = _require_str(power_input, "path_id", input_where)
            if path_id in seen_paths:
                raise InventoryError(f"{where} has duplicate path_id {path_id}")
            seen_paths.add(path_id)
            pdu_id = _require_str(power_input, "pdu_id", input_where)
            if pdu_id not in devices or devices[pdu_id]["device_type"] != "pdu":
                raise InventoryError(f"{input_where}.pdu_id references unknown PDU {pdu_id}")
            port = _require_int(power_input, "port", input_where, 1)
            if port > devices[pdu_id]["port_count"]:
                raise InventoryError(
                    f"{input_where}.port {port} exceeds {pdu_id} port_count"
                )
            declared_ups_id = _require_str(power_input, "declared_ups_id", input_where)
            if declared_ups_id not in devices or devices[declared_ups_id]["device_type"] != "ups":
                raise InventoryError(
                    f"{input_where}.declared_ups_id references unknown UPS {declared_ups_id}"
                )
            normalized_inputs.append(
                {
                    "path_id": path_id,
                    "pdu_id": pdu_id,
                    "port": port,
                    "declared_ups_id": declared_ups_id,
                    "declared_upstream_branch_id": power_input.get(
                        "declared_upstream_branch_id"
                    ),
                }
            )
        business_cost = item.get("business_cost", 0)
        if not isinstance(business_cost, (int, float)) or isinstance(business_cost, bool):
            raise InventoryError(f"{where}.business_cost must be a number")
        normalized_server = dict(item)
        normalized_server.update(
            {
                "server_id": server_id,
                "rack": item.get("rack"),
                "business_cost": float(business_cost),
                "acceptable_outage": bool(item.get("acceptable_outage", False)),
                "power_inputs": normalized_inputs,
            }
        )
        servers[server_id] = normalized_server
        server_order.append(server_id)

    return Inventory(
        raw=raw,
        devices=devices,
        servers=servers,
        server_order=tuple(server_order),
        device_order=tuple(device_order),
    )


def load_scenario(path: str | Path) -> dict[str, Any]:
    source = Path(path)
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise InventoryError(f"simulation scenario not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise InventoryError(f"invalid simulation scenario JSON: {exc}") from exc
    if not isinstance(raw, dict):
        raise InventoryError("simulation scenario root must be an object")
    if not isinstance(raw.get("devices", {}), dict):
        raise InventoryError("simulation scenario devices must be an object")
    return raw
