"""Concurrent SNMP topology collector with per-device and global deadlines."""

from __future__ import annotations

import concurrent.futures
import time
from typing import Any

from .config import AppConfig
from .constants import (
    DEVICE_ID,
    DEVICE_MODEL,
    DEVICE_STATUS,
    DEVICE_TYPE,
    PDU_PORT_BRANCH,
    PDU_PORT_SERVER,
    PDU_PORT_STATUS,
    PDU_PORT_UPS,
    PDU_UPLINK_BRANCH,
    PDU_UPLINK_UPS,
    STATUS_NAMES,
    SUPPLY_NAMES,
    SYS_DESCR,
    UPS_BATTERY_REMAINING,
    UPS_OUTPUT_LOAD,
    UPS_RUNTIME_MINUTES,
    UPS_SUPPLY_STATE,
)
from .inventory import Inventory
from .snmp_client import (
    DeviceEndpoint,
    SnmpClient,
    SnmpClientError,
    SnmpResponseError,
    SnmpTimeout,
)


class Collector:
    def __init__(self, config: AppConfig, inventory: Inventory) -> None:
        self.config = config
        self.inventory = inventory
        self.client = SnmpClient(
            max_varbinds_per_request=config.max_varbinds_per_request,
            retries=config.retries,
        )

    def _device_oids(self, device: dict[str, Any]) -> tuple[list[str], list[str]]:
        base = [
            SYS_DESCR,
            DEVICE_ID,
            DEVICE_TYPE,
            DEVICE_STATUS,
            DEVICE_MODEL,
        ]
        detail: list[str] = []
        if device["device_type"] == "pdu":
            detail.extend([PDU_UPLINK_UPS, PDU_UPLINK_BRANCH])
            for port in range(1, device["port_count"] + 1):
                detail.extend(
                    [
                        f"{PDU_PORT_UPS}.{port}",
                        f"{PDU_PORT_BRANCH}.{port}",
                        f"{PDU_PORT_STATUS}.{port}",
                        f"{PDU_PORT_SERVER}.{port}",
                    ]
                )
        else:
            detail.extend(
                [
                    UPS_OUTPUT_LOAD,
                    UPS_BATTERY_REMAINING,
                    UPS_RUNTIME_MINUTES,
                    UPS_SUPPLY_STATE,
                ]
            )
        return base, detail

    @staticmethod
    def _integer(value: Any) -> int | None:
        if isinstance(value, bool):
            return int(value)
        if isinstance(value, int):
            return value
        if isinstance(value, str) and value.strip():
            try:
                return int(value.strip())
            except ValueError:
                return None
        return None

    def _collect_one(
        self,
        device: dict[str, Any],
        *,
        device_deadline: float,
    ) -> dict[str, Any]:
        started = time.monotonic()
        result: dict[str, Any] = {
            "device_id": device["device_id"],
            "device_type": device["device_type"],
            "host": device["host"],
            "port": device["port"],
            "reachable": False,
            "online": False,
            "topology_known": False,
            "status": "unknown",
            "status_code": None,
            "error_code": None,
            "error": None,
            "sys_descr": None,
            "model": None,
            "uplink_ups_id": None,
            "upstream_branch_id": None,
            "ports": {},
            "output_load_percent": None,
            "battery_remaining_percent": None,
            "runtime_minutes": None,
            "supply_state": "unknown",
            "supply_state_code": None,
            "collection_ms": 0.0,
        }
        endpoint = DeviceEndpoint(device["host"], device["port"], device["community"])
        base_oids, detail_oids = self._device_oids(device)
        try:
            remaining = device_deadline - time.monotonic()
            if remaining <= 0:
                raise SnmpTimeout("device deadline exceeded")
            values = self.client.get(
                endpoint,
                base_oids,
                timeout=min(self.config.request_timeout_seconds, remaining),
                deadline=device_deadline,
            )
            result["reachable"] = True
            remaining = device_deadline - time.monotonic()
            if remaining <= 0:
                raise SnmpTimeout("device deadline exceeded while reading detail")
            values.update(
                self.client.get(
                    endpoint,
                    detail_oids,
                    timeout=min(self.config.request_timeout_seconds, remaining),
                    deadline=device_deadline,
                )
            )
        except SnmpTimeout as exc:
            result["error_code"] = "TIMEOUT"
            result["error"] = str(exc)
        except SnmpResponseError as exc:
            result["error_code"] = "SNMP_RESPONSE_ERROR"
            result["error"] = str(exc)
        except SnmpClientError as exc:
            result["error_code"] = "SNMP_ERROR"
            result["error"] = str(exc)
        except (OSError, ValueError) as exc:
            result["error_code"] = "TRANSPORT_ERROR"
            result["error"] = str(exc)
        else:
            result["sys_descr"] = values.get(SYS_DESCR)
            result["model"] = values.get(DEVICE_MODEL)
            status_code = self._integer(values.get(DEVICE_STATUS))
            result["status_code"] = status_code
            result["status"] = STATUS_NAMES.get(status_code, "unknown")
            result["online"] = result["reachable"] and status_code != 4
            if device["device_type"] == "pdu":
                result["uplink_ups_id"] = values.get(PDU_UPLINK_UPS) or None
                result["upstream_branch_id"] = values.get(PDU_UPLINK_BRANCH) or None
                for port in range(1, device["port_count"] + 1):
                    port_ups = values.get(f"{PDU_PORT_UPS}.{port}") or None
                    port_branch = values.get(f"{PDU_PORT_BRANCH}.{port}") or None
                    port_status = self._integer(values.get(f"{PDU_PORT_STATUS}.{port}"))
                    server_id = values.get(f"{PDU_PORT_SERVER}.{port}") or None
                    result["ports"][str(port)] = {
                        "port": port,
                        "upstream_ups_id": port_ups,
                        "upstream_branch_id": port_branch,
                        "status_code": port_status,
                        "status": (
                            "up"
                            if port_status == 1
                            else "down"
                            if port_status == 0
                            else "unknown"
                        ),
                        "connected_server_id": server_id,
                    }
                result["topology_known"] = bool(
                    result["online"]
                    and result["uplink_ups_id"]
                    and result["upstream_branch_id"]
                )
            else:
                result["output_load_percent"] = self._integer(
                    values.get(UPS_OUTPUT_LOAD)
                )
                result["battery_remaining_percent"] = self._integer(
                    values.get(UPS_BATTERY_REMAINING)
                )
                result["runtime_minutes"] = self._integer(
                    values.get(UPS_RUNTIME_MINUTES)
                )
                supply_code = self._integer(values.get(UPS_SUPPLY_STATE))
                result["supply_state_code"] = supply_code
                result["supply_state"] = SUPPLY_NAMES.get(supply_code, "unknown")
                result["topology_known"] = bool(result["online"])
        if result["error_code"] is not None:
            # A partially read device must not be treated as trustworthy.
            result["reachable"] = False
            result["online"] = False
            result["topology_known"] = False
            result["status"] = "unknown"
            result["status_code"] = None
            result["ports"] = {}
            result["uplink_ups_id"] = None
            result["upstream_branch_id"] = None
            result["output_load_percent"] = None
            result["battery_remaining_percent"] = None
            result["runtime_minutes"] = None
            result["supply_state"] = "unknown"
            result["supply_state_code"] = None
        result["collection_ms"] = round((time.monotonic() - started) * 1000.0, 3)
        return result

    def collect(self) -> dict[str, Any]:
        started = time.monotonic()
        # Keep a small scheduling margin so the observable audit duration
        # remains within the configured hard limit after result assembly.
        budget = max(
            0.001,
            self.config.total_timeout_seconds
            - min(0.02, self.config.total_timeout_seconds * 0.05),
        )
        total_deadline = started + budget
        results: dict[str, dict[str, Any]] = {}
        executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=min(self.config.max_workers, max(1, len(self.inventory.device_order))),
            thread_name_prefix="snmp-poll",
        )
        futures: dict[concurrent.futures.Future[dict[str, Any]], str] = {}
        try:
            for device_id in self.inventory.device_order:
                device = self.inventory.devices[device_id]
                per_device_deadline = min(
                    total_deadline,
                    time.monotonic() + self.config.device_timeout_seconds,
                )
                futures[
                    executor.submit(
                        self._collect_one,
                        device,
                        device_deadline=per_device_deadline,
                    )
                ] = device_id
            remaining = max(0.0, total_deadline - time.monotonic())
            done, pending = concurrent.futures.wait(futures, timeout=remaining)
            for future in done:
                device_id = futures[future]
                try:
                    results[device_id] = future.result()
                except Exception as exc:  # Defensive isolation per device.
                    results[device_id] = self._failed_result(
                        self.inventory.devices[device_id],
                        "COLLECTOR_ERROR",
                        str(exc),
                    )
            for future in pending:
                device_id = futures[future]
                future.cancel()
                results[device_id] = self._failed_result(
                    self.inventory.devices[device_id],
                    "TOTAL_TIMEOUT",
                    "audit deadline exceeded",
                )
        finally:
            executor.shutdown(wait=False, cancel_futures=True)

        ordered = {
            device_id: results.get(
                device_id,
                self._failed_result(
                    self.inventory.devices[device_id],
                    "COLLECTOR_ERROR",
                    "device result missing",
                ),
            )
            for device_id in self.inventory.device_order
        }
        return {
            "started_monotonic": started,
            "duration_ms": round((time.monotonic() - started) * 1000.0, 3),
            "configured_total_timeout_seconds": self.config.total_timeout_seconds,
            "effective_deadline_seconds": budget,
            "devices": ordered,
        }

    @staticmethod
    def _failed_result(
        device: dict[str, Any], error_code: str, error: str
    ) -> dict[str, Any]:
        return {
            "device_id": device["device_id"],
            "device_type": device["device_type"],
            "host": device["host"],
            "port": device["port"],
            "reachable": False,
            "online": False,
            "topology_known": False,
            "status": "unknown",
            "status_code": None,
            "error_code": error_code,
            "error": error,
            "sys_descr": None,
            "model": None,
            "uplink_ups_id": None,
            "upstream_branch_id": None,
            "ports": {},
            "output_load_percent": None,
            "battery_remaining_percent": None,
            "runtime_minutes": None,
            "supply_state": "unknown",
            "supply_state_code": None,
            "collection_ms": 0.0,
        }
