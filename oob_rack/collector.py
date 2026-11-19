from __future__ import annotations

import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from .config import resolve_path
from .errors import ProtocolError
from .ipmi import IpmiClient
from .normalization import (
    aggregate_device,
    normalize_protocol_payload,
    protocol_failure,
)
from .redfish_client import RedfishClient
from .snmp import SnmpClient


def protocol_order(device: dict, profile: dict) -> list[str]:
    supported = list(profile["protocols"])
    declared = [
        name for name in device.get("fallback_protocols", []) if name in supported
    ]
    return declared + [name for name in supported if name not in declared]


class RackCollector:
    def __init__(self, config: dict):
        self.config = config
        self.inspection = config["inspection"]
        self.workers = min(2, max(1, int(self.inspection.get("workers", 2))))
        self.timeout = float(self.inspection["timeout_seconds"])
        self.retries = int(self.inspection["retries"])
        self.host = config["listen_host"]
        self.ca_file = str(resolve_path(config, config["redfish"]["ca_file"]))

    def _collect_ipmi(self, device: dict) -> dict:
        client = IpmiClient(
            self.host,
            device["ipmi_port"],
            self.config["ipmi"]["username"],
            self.config["ipmi"]["password"],
            timeout=self.timeout,
            retries=self.retries,
        )
        try:
            client.open()
            return client.collect_payload()
        finally:
            client.close()

    def _collect_redfish(self, device: dict) -> dict:
        client = RedfishClient(
            self.host,
            device["redfish_port"],
            self.config["redfish"]["username"],
            self.config["redfish"]["password"],
            self.ca_file,
            timeout=self.timeout,
            retries=self.retries,
        )
        return client.collect_payload()

    def _collect_snmp(self, device: dict) -> dict:
        client = SnmpClient(
            self.host,
            device["snmp_port"],
            self.config["snmp"]["community"],
            self.config["snmp"]["enterprise_base"],
            timeout=self.timeout,
            retries=self.retries,
        )
        with client:
            return client.collect_payload()

    def collect_protocol(self, device: dict, protocol: str) -> dict:
        started = time.monotonic()
        try:
            if protocol == "ipmi":
                payload = self._collect_ipmi(device)
            elif protocol == "redfish":
                payload = self._collect_redfish(device)
            elif protocol == "snmp":
                payload = self._collect_snmp(device)
            else:
                raise ProtocolError(protocol, "unsupported protocol")
            profile = self.config["firmware_profiles"][device["firmware_profile"]]
            thresholds = self.config["thresholds"][device["firmware_profile"]]
            result = normalize_protocol_payload(
                protocol,
                payload,
                device_id=device["device_id"],
                profile=profile,
                thresholds=thresholds,
            )
        except ProtocolError as exc:
            result = protocol_failure(protocol, exc.reason, raw=exc.details)
        except Exception as exc:
            result = protocol_failure(protocol, f"{type(exc).__name__}: {exc}")
        result["duration_ms"] = round((time.monotonic() - started) * 1000, 1)
        return result

    def collect_device(self, device: dict) -> dict:
        started = time.monotonic()
        profile = self.config["firmware_profiles"][device["firmware_profile"]]
        order = protocol_order(device, profile)
        protocol_results = {}
        for protocol in order:
            protocol_results[protocol] = self.collect_protocol(device, protocol)
        aggregate = aggregate_device(
            device=device,
            profile=profile,
            protocol_results=protocol_results,
            protocol_order=order,
        )
        aggregate["duration_ms"] = round((time.monotonic() - started) * 1000, 1)
        return aggregate

    def collect_rack(self, device_ids: set[str] | None = None) -> dict:
        started = time.monotonic()
        results: dict[str, dict] = {}
        selected_devices = [
            device
            for device in self.config["devices"]
            if device_ids is None or device["device_id"] in device_ids
        ]
        with ThreadPoolExecutor(
            max_workers=self.workers, thread_name_prefix="oob-collect"
        ) as executor:
            futures = {
                executor.submit(self.collect_device, device): device["device_id"]
                for device in selected_devices
            }
            for future in as_completed(futures):
                device_id = futures[future]
                try:
                    results[device_id] = future.result()
                except Exception as exc:
                    device = next(
                        item for item in self.config["devices"] if item["device_id"] == device_id
                    )
                    profile = self.config["firmware_profiles"][device["firmware_profile"]]
                    order = protocol_order(device, profile)
                    protocol_results = {
                        name: protocol_failure(name, f"collector crashed: {exc}")
                        for name in order
                    }
                    aggregate = aggregate_device(
                        device=device,
                        profile=profile,
                        protocol_results=protocol_results,
                        protocol_order=order,
                    )
                    aggregate["duration_ms"] = 0
                    results[device_id] = aggregate
        ordered = [results[device["device_id"]] for device in selected_devices]
        return {
            "rack_id": self.config["rack_id"],
            "devices": ordered,
            "duration_ms": round((time.monotonic() - started) * 1000, 1),
        }
