from __future__ import annotations

import threading
from dataclasses import dataclass, field

from .errors import ConfigSchemaError
from .deterministic import deterministic_power, deterministic_sensors, seeded_rng

VALID_FAULT_KINDS = {
    "offline",
    "sensor_override",
    "power_override",
    "redfish_power_unparsable",
    "redfish_sensor_unparsable",
}


@dataclass
class FaultController:
    _next_id: int = 1
    _faults: dict[int, dict] = field(default_factory=dict)
    _lock: threading.RLock = field(default_factory=threading.RLock)

    def add(self, fault: dict) -> dict:
        if not isinstance(fault, dict):
            raise ConfigSchemaError("fault must be a JSON object")
        kind = fault.get("kind")
        if kind not in VALID_FAULT_KINDS:
            raise ConfigSchemaError(f"unsupported fault kind: {kind!r}")
        if kind == "offline" and fault.get("protocol") not in ("ipmi", "redfish", "snmp", "all"):
            raise ConfigSchemaError("offline fault needs protocol=ipmi|redfish|snmp|all")
        if kind in ("sensor_override", "redfish_sensor_unparsable"):
            if not isinstance(fault.get("sensor"), str):
                raise ConfigSchemaError(f"{kind} fault needs a sensor name")
        if kind == "sensor_override" and not isinstance(fault.get("value"), (int, float)):
            raise ConfigSchemaError("sensor_override fault needs a numeric value")
        if kind == "power_override":
            if fault.get("protocol") not in ("ipmi", "redfish", "snmp"):
                raise ConfigSchemaError("power_override fault needs a protocol")
            if str(fault.get("value", "")).lower() not in ("on", "off", "unknown"):
                raise ConfigSchemaError("power_override value must be on, off, or unknown")
        with self._lock:
            fault_id = self._next_id
            self._next_id += 1
            stored = dict(fault)
            stored["id"] = fault_id
            self._faults[fault_id] = stored
            return dict(stored)

    def clear(self, *, fault_id: int | None = None, device_id: str | None = None) -> int:
        with self._lock:
            if fault_id is not None:
                return 1 if self._faults.pop(fault_id, None) is not None else 0
            if device_id is None:
                count = len(self._faults)
                self._faults.clear()
                return count
            keep = {
                key: value
                for key, value in self._faults.items()
                if value.get("device_id") != device_id
            }
            count = len(self._faults) - len(keep)
            self._faults = keep
            return count

    def list(self, device_id: str | None = None) -> list[dict]:
        with self._lock:
            values = [dict(value) for value in self._faults.values()]
        if device_id is not None:
            values = [value for value in values if value.get("device_id") == device_id]
        return sorted(values, key=lambda item: item["id"])

    def _matching(self, device_id: str, kind: str) -> list[dict]:
        with self._lock:
            return [
                dict(value)
                for value in self._faults.values()
                if value.get("device_id") in (None, device_id) and value.get("kind") == kind
            ]

    def is_offline(self, device_id: str, protocol: str) -> bool:
        for fault in self._matching(device_id, "offline"):
            if fault.get("protocol") in (protocol, "all"):
                return True
        return False

    def sensor_override(self, device_id: str, sensor: str):
        value = None
        for fault in self._matching(device_id, "sensor_override"):
            if fault.get("sensor") == sensor:
                value = fault.get("value")
        return value

    def power_override(self, device_id: str, protocol: str):
        value = None
        for fault in self._matching(device_id, "power_override"):
            if fault.get("protocol") == protocol:
                value = str(fault.get("value")).lower()
        return value

    def has(self, device_id: str, kind: str, **criteria) -> bool:
        for fault in self._matching(device_id, kind):
            if all(fault.get(key) == value for key, value in criteria.items()):
                return True
        return False


class BmcRuntime:
    def __init__(self, device: dict, profile: dict, seed: int, faults: FaultController):
        self.device = device
        self.profile = profile
        self.seed = seed
        self.faults = faults
        self.device_id = device["device_id"]
        self.firmware = device["firmware_version"]
        self._base_power = deterministic_power(seed, self.device_id, self.firmware)
        self._base_sensors = deterministic_sensors(seed, self.device_id, self.firmware)
        self._lock = threading.RLock()
        rng = seeded_rng(seed, self.device_id, "psu")
        self._psu_watts = [rng.randint(180, 420), rng.randint(180, 420)]

    @property
    def asset(self) -> dict:
        return {
            "device_id": self.device_id,
            "manufacturer": self.device["manufacturer"],
            "model": self.device["model"],
            "serial": self.device["serial"],
            "firmware_version": self.firmware,
            "rack_id": self.device["rack_id"],
            "slot": self.device["slot"],
        }

    def power(self, protocol: str) -> str:
        override = self.faults.power_override(self.device_id, protocol)
        if override is not None:
            return override
        return self._base_power

    def sensors(self, protocol: str | None = None) -> dict[str, int]:
        with self._lock:
            result = dict(self._base_sensors)
        for name in list(result):
            override = self.faults.sensor_override(self.device_id, name)
            if override is not None:
                result[name] = override
        return result

    def redfish_sensor_is_unparsable(self, sensor: str) -> bool:
        return self.faults.has(
            self.device_id, "redfish_sensor_unparsable", sensor=sensor
        )

    def redfish_power_is_unparsable(self) -> bool:
        return self.faults.has(self.device_id, "redfish_power_unparsable")

    def is_offline(self, protocol: str) -> bool:
        return self.faults.is_offline(self.device_id, protocol)

    def power_supplies(self) -> list[dict]:
        result = []
        for index, name in enumerate(self.device["power_supplies"]):
            result.append(
                {
                    "name": name,
                    "member_id": name.replace(" ", ""),
                    "watts": self._psu_watts[index],
                    "health": "OK",
                    "state": "Enabled",
                }
            )
        return result
