from __future__ import annotations

import hashlib
import json
import math
import os
import threading
from copy import deepcopy
from pathlib import Path

from .errors import (
    ConfigError,
    ConfigImmutableError,
    ConfigReadError,
    ConfigSchemaError,
    ThresholdConfigError,
)

IPMI_PORT_RANGE = (6230, 6235)
REDFISH_PORT_RANGE = (8443, 8448)
SNMP_PORT_RANGE = (1610, 1615)
SUPPORTED_PROTOCOLS = ("ipmi", "redfish", "snmp")

IMMUTABLE_KEYS = (
    "seed",
    "rack_id",
    "listen_host",
    "devices",
    "firmware_profiles",
    "monitor",
    "control",
    "redfish",
    "ipmi",
    "snmp",
)


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ConfigSchemaError(message)


def _number(value, name: str, *, minimum=None, integer=False) -> float | int:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ThresholdConfigError(f"{name} must be numeric", details={"field": name})
    if not math.isfinite(float(value)):
        raise ThresholdConfigError(f"{name} must be finite", details={"field": name})
    if integer and int(value) != value:
        raise ThresholdConfigError(f"{name} must be an integer", details={"field": name})
    if minimum is not None and float(value) < float(minimum):
        raise ThresholdConfigError(
            f"{name} must be >= {minimum}", details={"field": name, "minimum": minimum}
        )
    return int(value) if integer else float(value)


def _validate_threshold_block(profile_name: str, block: dict) -> None:
    _require(isinstance(block, dict), f"profile {profile_name}: thresholds must be an object")
    temp = block.get("temperature_c")
    fan = block.get("fan_rpm")
    _require(isinstance(temp, dict), f"profile {profile_name}: temperature_c is required")
    _require(isinstance(fan, dict), f"profile {profile_name}: fan_rpm is required")
    temp_lower = _number(temp.get("lower"), f"{profile_name}.temperature_c.lower")
    temp_upper = _number(temp.get("upper"), f"{profile_name}.temperature_c.upper")
    fan_lower = _number(
        fan.get("lower"), f"{profile_name}.fan_rpm.lower", minimum=0, integer=True
    )
    fan_upper = _number(
        fan.get("upper"), f"{profile_name}.fan_rpm.upper", minimum=0, integer=True
    )
    if temp_lower >= temp_upper:
        raise ThresholdConfigError(
            f"profile {profile_name}: temperature lower must be below upper",
            details={
                "profile": profile_name,
                "temperature_lower": temp_lower,
                "temperature_upper": temp_upper,
            },
        )
    if fan_lower > fan_upper:
        raise ThresholdConfigError(
            f"profile {profile_name}: fan lower must not exceed upper",
            details={
                "profile": profile_name,
                "fan_lower": fan_lower,
                "fan_upper": fan_upper,
            },
        )


def validate_config(config: dict) -> None:
    _require(isinstance(config, dict), "configuration root must be an object")
    for key in (
        "rack_id",
        "seed",
        "listen_host",
        "devices",
        "firmware_profiles",
        "thresholds",
        "inspection",
        "ipmi",
        "redfish",
        "snmp",
        "monitor",
        "control",
    ):
        _require(key in config, f"missing required key: {key}")
    _require(config["listen_host"] == "127.0.0.1", "listen_host must be 127.0.0.1")
    _require(isinstance(config["seed"], int) and not isinstance(config["seed"], bool), "seed must be an integer")
    _require(isinstance(config["rack_id"], str) and config["rack_id"], "rack_id must be non-empty")
    _require(isinstance(config["devices"], list) and config["devices"], "devices must be a non-empty array")
    _require(isinstance(config["firmware_profiles"], dict), "firmware_profiles must be an object")
    _require(isinstance(config.get("thresholds"), dict), "thresholds must be an object")

    for name, profile in config["firmware_profiles"].items():
        _require(isinstance(profile, dict), f"profile {name} must be an object")
        protocols = profile.get("protocols")
        _require(
            isinstance(protocols, list) and protocols,
            f"profile {name}: protocols must be a non-empty array",
        )
        _require(
            all(item in SUPPORTED_PROTOCOLS for item in protocols),
            f"profile {name}: unsupported protocol in protocols",
        )
        _require(len(protocols) == len(set(protocols)), f"profile {name}: duplicate protocol")
        shape = profile.get("power_state_shape")
        _require(shape in ("none", "string", "object"), f"profile {name}: invalid power_state_shape")
        if "redfish" in protocols:
            _require(shape in ("string", "object"), f"profile {name}: Redfish profile needs a power shape")
        else:
            _require(shape == "none", f"profile {name}: non-Redfish profile must use none")
        _require(name in config["thresholds"], f"profile {name}: thresholds are required")
        _validate_threshold_block(name, config["thresholds"][name])

    extra_thresholds = set(config["thresholds"]) - set(config["firmware_profiles"])
    _require(not extra_thresholds, f"thresholds reference unknown profiles: {sorted(extra_thresholds)}")

    seen_ids: set[str] = set()
    used: dict[str, set[int]] = {"ipmi": set(), "redfish": set(), "snmp": set()}
    ranges = {
        "ipmi": IPMI_PORT_RANGE,
        "redfish": REDFISH_PORT_RANGE,
        "snmp": SNMP_PORT_RANGE,
    }
    for index, device in enumerate(config["devices"]):
        _require(isinstance(device, dict), f"device[{index}] must be an object")
        device_id = device.get("device_id")
        _require(isinstance(device_id, str) and device_id, f"device[{index}].device_id is required")
        _require(device_id not in seen_ids, f"duplicate device_id: {device_id}")
        seen_ids.add(device_id)
        profile_name = device.get("firmware_profile")
        _require(
            profile_name in config["firmware_profiles"],
            f"{device_id}: unknown firmware_profile",
        )
        _require(
            isinstance(device.get("power_supplies"), list) and len(device["power_supplies"]) == 2,
            f"{device_id}: exactly two power_supplies are required",
        )
        _require(
            isinstance(device.get("fans"), list) and len(device["fans"]) == 4,
            f"{device_id}: exactly four fans are required",
        )
        fallback = device.get("fallback_protocols")
        _require(isinstance(fallback, list) and fallback, f"{device_id}: fallback_protocols required")
        _require(
            all(item in SUPPORTED_PROTOCOLS for item in fallback),
            f"{device_id}: unsupported fallback protocol",
        )
        for protocol, key in (
            ("ipmi", "ipmi_port"),
            ("redfish", "redfish_port"),
            ("snmp", "snmp_port"),
        ):
            port = device.get(key)
            _require(
                isinstance(port, int) and not isinstance(port, bool),
                f"{device_id}: {key} must be an integer",
            )
            low, high = ranges[protocol]
            _require(low <= port <= high, f"{device_id}: {key} outside {low}-{high}")
            _require(port not in used[protocol], f"{device_id}: duplicate {protocol} port {port}")
            used[protocol].add(port)

    inspection = config["inspection"]
    _require(isinstance(inspection, dict), "inspection must be an object")
    _require(
        isinstance(inspection.get("interval_seconds"), (int, float))
        and inspection["interval_seconds"] > 0,
        "inspection.interval_seconds must be > 0",
    )
    _require(
        isinstance(inspection.get("timeout_seconds"), (int, float))
        and inspection["timeout_seconds"] > 0,
        "inspection.timeout_seconds must be > 0",
    )
    _require(
        isinstance(inspection.get("retries"), int) and 0 <= inspection["retries"] <= 10,
        "inspection.retries must be 0..10",
    )
    _require(
        isinstance(inspection.get("workers"), int) and 1 <= inspection["workers"] <= 2,
        "inspection.workers must be 1..2 (resource contract)",
    )
    _require(isinstance(inspection.get("database"), str) and inspection["database"], "inspection.database required")
    for item in ("monitor", "control", "ipmi", "redfish", "snmp"):
        _require(isinstance(config[item], dict), f"{item} must be an object")
    for item in ("monitor", "control"):
        _require(config[item].get("host") == "127.0.0.1", f"{item}.host must be 127.0.0.1")
        port = config[item].get("port")
        _require(isinstance(port, int) and 1 <= port <= 65535, f"{item}.port is invalid")
    _require(config["redfish"].get("verify_tls") is True, "redfish.verify_tls must be true")
    for key in ("ca_file", "server_cert", "server_key", "username", "password"):
        _require(config["redfish"].get(key), f"redfish.{key} is required")
    for key in ("username", "password"):
        _require(config["ipmi"].get(key), f"ipmi.{key} is required")
    _require(config["snmp"].get("community"), "snmp.community is required")


def load_config(path: str | os.PathLike) -> dict:
    config_path = Path(path)
    try:
        with config_path.open("r", encoding="utf-8") as handle:
            config = json.load(handle)
    except FileNotFoundError as exc:
        raise ConfigReadError("configuration file not found") from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise ConfigReadError(f"configuration could not be read: {exc}") from exc
    validate_config(config)
    config["_config_path"] = str(config_path)
    config["_config_dir"] = str(config_path.resolve().parent)
    return config


def canonical_fingerprint(config: dict) -> str:
    public = {key: value for key, value in config.items() if not key.startswith("_")}
    encoded = json.dumps(public, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def resolve_path(config: dict, value: str) -> Path:
    path = Path(value)
    if path.is_absolute():
        return path
    return Path(config.get("_config_dir", ".")) / path


class ConfigManager:
    """Holds the active configuration; failed reloads never replace it."""

    def __init__(self, path: str | os.PathLike):
        self.path = str(path)
        self._config = load_config(self.path)
        self._fingerprint = canonical_fingerprint(self._config)
        self.generation = 1
        self._lock = threading.RLock()

    @property
    def config(self) -> dict:
        with self._lock:
            return self._config

    @property
    def fingerprint(self) -> str:
        return self._fingerprint

    def reload(self) -> dict:
        try:
            candidate = load_config(self.path)
        except ConfigError:
            raise
        with self._lock:
            old = self._config
            for key in IMMUTABLE_KEYS:
                if candidate.get(key) != old.get(key):
                    raise ConfigImmutableError(
                        f"runtime reload cannot change immutable key: {key}",
                        details={"key": key},
                    )
            self._config = candidate
            self._fingerprint = canonical_fingerprint(candidate)
            self.generation += 1
            return deepcopy(candidate)
