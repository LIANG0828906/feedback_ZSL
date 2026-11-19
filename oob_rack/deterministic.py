from __future__ import annotations

import hashlib
import random
from dataclasses import dataclass


@dataclass(frozen=True)
class SensorSpec:
    name: str
    kind: str
    unit: str
    lower: float
    upper: float


TEMPERATURE_SPECS = (
    SensorSpec("Inlet Temp", "temperature", "C", 5.0, 70.0),
    SensorSpec("CPU Temp", "temperature", "C", 5.0, 75.0),
)

FAN_SPECS = (
    SensorSpec("Fan 1", "fan", "RPM", 600, 8000),
    SensorSpec("Fan 2", "fan", "RPM", 600, 8000),
    SensorSpec("Fan 3", "fan", "RPM", 600, 8000),
    SensorSpec("Fan 4", "fan", "RPM", 600, 8000),
)

SENSOR_SPECS = TEMPERATURE_SPECS + FAN_SPECS


def stable_int(*parts: object) -> int:
    digest = hashlib.sha256("\x1f".join(str(part) for part in parts).encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big", signed=False)


def seeded_rng(seed: int, *parts: object) -> random.Random:
    return random.Random(stable_int(seed, *parts))


def deterministic_sensors(seed: int, device_id: str, firmware: str) -> dict[str, int]:
    """Return restart-stable integer readings for one BMC."""
    result: dict[str, int] = {}
    for spec in SENSOR_SPECS:
        rng = seeded_rng(seed, device_id, firmware, spec.name)
        if spec.kind == "temperature":
            result[spec.name] = rng.randint(24, 69)
        else:
            result[spec.name] = rng.randint(72, 238) * 10
    return result


def deterministic_power(seed: int, device_id: str, firmware: str) -> str:
    rng = seeded_rng(seed, device_id, firmware, "power")
    return "on" if rng.random() < 0.75 else "off"


def deterministic_guid(device_id: str) -> bytes:
    return hashlib.sha256(f"bmc-guid:{device_id}".encode("utf-8")).digest()[:16]
