#!/usr/bin/env python3
"""Generate deterministic 96-server inventory and simulator scenario."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
DATA = ROOT / "data"


def main() -> None:
    DATA.mkdir(parents=True, exist_ok=True)
    devices: list[dict[str, Any]] = []
    scenario_devices: dict[str, dict[str, Any]] = {}
    ups_ids = [f"UPS-{index:02d}" for index in range(1, 5)]
    ups_values = {
        "UPS-01": {
            "status": "on_battery",
            "output_load_percent": 63,
            "battery_remaining_percent": 18,
            "runtime_minutes": 12,
            "supply_state": "battery",
        },
        "UPS-02": {
            "status": "online",
            "output_load_percent": 71,
            "battery_remaining_percent": 85,
            "runtime_minutes": 45,
            "supply_state": "mains",
        },
        "UPS-03": {
            "status": "online",
            "output_load_percent": 94,
            "battery_remaining_percent": 80,
            "runtime_minutes": 40,
            "supply_state": "mains",
        },
        "UPS-04": {
            "status": "online",
            "output_load_percent": 66,
            "battery_remaining_percent": 88,
            "runtime_minutes": 52,
            "supply_state": "mains",
        },
    }
    for offset, ups_id in enumerate(ups_ids):
        devices.append(
            {
                "device_id": ups_id,
                "device_type": "ups",
                "host": "127.0.0.1",
                "port": 12701 + offset,
                "community": "public",
                "enabled": True,
            }
        )
        scenario_devices[ups_id] = dict(ups_values[ups_id])

    pdu_specs: list[dict[str, Any]] = []
    for rack in range(1, 9):
        for side, ups_id in (
            ("A", "UPS-01" if rack <= 4 else "UPS-02"),
            ("B", "UPS-03" if rack <= 4 else "UPS-04"),
        ):
            pdu_specs.append(
                {
                    "device_id": f"PDU-R{rack:02d}-{side}",
                    "rack": rack,
                    "side": side,
                    "uplink_ups_id": ups_id,
                    "branch": f"FEED-R{rack:02d}-{side}",
                    "port_count": 24,
                }
            )
    pdu_specs.append(
        {
            "device_id": "PDU-AUX-17",
            "rack": None,
            "side": None,
            "uplink_ups_id": "UPS-01",
            "branch": "FEED-UPS01-AUX",
            "port_count": 32,
        }
    )
    for offset, spec in enumerate(pdu_specs):
        devices.append(
            {
                "device_id": spec["device_id"],
                "device_type": "pdu",
                "host": "127.0.0.1",
                "port": 12705 + offset,
                "community": "public",
                "enabled": True,
                "port_count": spec["port_count"],
            }
        )
        scenario_devices[spec["device_id"]] = {
            "status": "online",
            "uplink_ups_id": spec["uplink_ups_id"],
            "uplink_branch_id": spec["branch"],
            "ports": {},
        }

    devices_by_id = {device["device_id"]: device for device in devices}
    ports_used = {device_id: 0 for device_id in devices_by_id}
    actual: dict[str, dict[str, dict[str, Any]]] = {}
    servers: list[dict[str, Any]] = []

    def allocate(pdu_id: str) -> int:
        ports_used[pdu_id] += 1
        port = ports_used[pdu_id]
        if port > devices_by_id[pdu_id]["port_count"]:
            raise RuntimeError(f"port capacity exhausted for {pdu_id}")
        return port

    def connect(
        pdu_id: str,
        port: int,
        server_id: str,
        ups_id: str,
        branch: str,
    ) -> None:
        actual.setdefault(pdu_id, {})[str(port)] = {
            "server_id": server_id,
            "status": "up",
            "upstream_ups_id": ups_id,
            "upstream_branch_id": branch,
        }
        scenario_devices[pdu_id]["ports"] = actual[pdu_id]

    def add_server(
        number: int,
        rack: int,
        power_inputs: list[dict[str, Any]],
    ) -> None:
        server_id = f"SRV-{number:03d}"
        servers.append(
            {
                "server_id": server_id,
                "rack": f"R{rack:02d}",
                "business_cost": (number * 37) % 997 + 1,
                "acceptable_outage": number in {11, 29, 47, 65, 83, 92, 95},
                "power_inputs": power_inputs,
            }
        )

    number = 1
    spec_by_id = {spec["device_id"]: spec for spec in pdu_specs}

    # 66 correctly independent dual-path servers.
    for index in range(66):
        rack = index % 8 + 1
        pdu_a = f"PDU-R{rack:02d}-A"
        pdu_b = f"PDU-R{rack:02d}-B"
        spec_a = spec_by_id[pdu_a]
        spec_b = spec_by_id[pdu_b]
        port_a = allocate(pdu_a)
        port_b = allocate(pdu_b)
        server_id = f"SRV-{number:03d}"
        connect(pdu_a, port_a, server_id, spec_a["uplink_ups_id"], spec_a["branch"])
        connect(pdu_b, port_b, server_id, spec_b["uplink_ups_id"], spec_b["branch"])
        add_server(
            number,
            rack,
            [
                {
                    "path_id": "A",
                    "pdu_id": pdu_a,
                    "port": port_a,
                    "declared_ups_id": spec_a["uplink_ups_id"],
                    "declared_upstream_branch_id": spec_a["branch"],
                },
                {
                    "path_id": "B",
                    "pdu_id": pdu_b,
                    "port": port_b,
                    "declared_ups_id": spec_b["uplink_ups_id"],
                    "declared_upstream_branch_id": spec_b["branch"],
                },
            ],
        )
        number += 1

    # 6 servers on two different PDUs that share UPS-02.
    for index in range(6):
        pdu_a = "PDU-R05-A" if index % 2 == 0 else "PDU-R06-A"
        pdu_b = "PDU-R06-A" if index % 2 == 0 else "PDU-R05-A"
        spec_a = spec_by_id[pdu_a]
        spec_b = spec_by_id[pdu_b]
        port_a = allocate(pdu_a)
        port_b = allocate(pdu_b)
        server_id = f"SRV-{number:03d}"
        connect(pdu_a, port_a, server_id, spec_a["uplink_ups_id"], spec_a["branch"])
        connect(pdu_b, port_b, server_id, spec_b["uplink_ups_id"], spec_b["branch"])
        add_server(
            number,
            5 + index % 2,
            [
                {
                    "path_id": "A",
                    "pdu_id": pdu_a,
                    "port": port_a,
                    "declared_ups_id": spec_a["uplink_ups_id"],
                    "declared_upstream_branch_id": spec_a["branch"],
                },
                {
                    "path_id": "B",
                    "pdu_id": pdu_b,
                    "port": port_b,
                    "declared_ups_id": spec_b["uplink_ups_id"],
                    "declared_upstream_branch_id": spec_b["branch"],
                },
            ],
        )
        number += 1

    # 4 servers on different PDUs/UPSes that share the same physical branch.
    for index in range(4):
        pdu_a = "PDU-R04-A"
        pdu_b = "PDU-R05-A"
        spec_a = spec_by_id[pdu_a]
        spec_b = spec_by_id[pdu_b]
        port_a = allocate(pdu_a)
        port_b = allocate(pdu_b)
        server_id = f"SRV-{number:03d}"
        connect(
            pdu_a,
            port_a,
            server_id,
            spec_a["uplink_ups_id"],
            "FEED-R04R05-SHARED",
        )
        connect(
            pdu_b,
            port_b,
            server_id,
            spec_b["uplink_ups_id"],
            "FEED-R04R05-SHARED",
        )
        add_server(
            number,
            4,
            [
                {
                    "path_id": "A",
                    "pdu_id": pdu_a,
                    "port": port_a,
                    "declared_ups_id": spec_a["uplink_ups_id"],
                    "declared_upstream_branch_id": "FEED-R04R05-SHARED",
                },
                {
                    "path_id": "B",
                    "pdu_id": pdu_b,
                    "port": port_b,
                    "declared_ups_id": spec_b["uplink_ups_id"],
                    "declared_upstream_branch_id": "FEED-R04R05-SHARED",
                },
            ],
        )
        number += 1

    # 6 single-path servers.
    for index in range(6):
        rack = index + 1
        pdu_id = f"PDU-R{rack:02d}-A"
        spec = spec_by_id[pdu_id]
        port = allocate(pdu_id)
        server_id = f"SRV-{number:03d}"
        connect(pdu_id, port, server_id, spec["uplink_ups_id"], spec["branch"])
        add_server(
            number,
            rack,
            [
                {
                    "path_id": "A",
                    "pdu_id": pdu_id,
                    "port": port,
                    "declared_ups_id": spec["uplink_ups_id"],
                    "declared_upstream_branch_id": spec["branch"],
                }
            ],
        )
        number += 1

    # 14 actual same-PDU paths with intentionally stale, apparently
    # redundant inventory declarations.
    for index in range(14):
        pdu_id = "PDU-AUX-17"
        spec = spec_by_id[pdu_id]
        port_a = allocate(pdu_id)
        port_b = allocate(pdu_id)
        server_id = f"SRV-{number:03d}"
        connect(pdu_id, port_a, server_id, spec["uplink_ups_id"], spec["branch"])
        connect(pdu_id, port_b, server_id, spec["uplink_ups_id"], spec["branch"])
        declared_rack = index % 8 + 1
        declared_a = f"PDU-R{declared_rack:02d}-A"
        declared_b = f"PDU-R{declared_rack:02d}-B"
        spec_a = spec_by_id[declared_a]
        spec_b = spec_by_id[declared_b]
        add_server(
            number,
            declared_rack,
            [
                {
                    "path_id": "A",
                    "pdu_id": declared_a,
                    "port": min(20 + index, devices_by_id[declared_a]["port_count"]),
                    "declared_ups_id": spec_a["uplink_ups_id"],
                    "declared_upstream_branch_id": spec_a["branch"],
                },
                {
                    "path_id": "B",
                    "pdu_id": declared_b,
                    "port": min(20 + index, devices_by_id[declared_b]["port_count"]),
                    "declared_ups_id": spec_b["uplink_ups_id"],
                    "declared_upstream_branch_id": spec_b["branch"],
                },
            ],
        )
        number += 1

    inventory = {
        "inventory_version": 1,
        "site": "edge-lab",
        "description": "96 servers, 4 UPS, 17 PDU demo inventory",
        "devices": devices,
        "servers": servers,
    }
    scenario = {
        "scenario_version": 1,
        "description": "Deterministic local SNMP device state",
        "devices": scenario_devices,
    }
    (DATA / "asset_inventory.json").write_text(
        json.dumps(inventory, ensure_ascii=False, indent=2, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    (DATA / "simulator_scenario.json").write_text(
        json.dumps(scenario, ensure_ascii=False, indent=2, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    print(
        f"generated {len(servers)} servers, {len(devices)} devices, "
        f"{sum(len(ports) for ports in actual.values())} connected ports"
    )


if __name__ == "__main__":
    main()
