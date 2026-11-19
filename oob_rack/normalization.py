from __future__ import annotations

import json
import math
from typing import Any

VALID_POWER = ("on", "off", "unknown")


def raw_snippet(value: Any, limit: int = 512) -> str:
    try:
        encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    except TypeError:
        encoded = repr(value)
    return encoded[:limit]


def _unparsable(protocol: str, path: str, raw: Any, reason: str) -> dict:
    return {
        "protocol": protocol,
        "path": path,
        "raw_snippet": raw_snippet(raw),
        "reason": reason,
    }


def map_power_label(value: Any) -> str:
    text = str(value).strip().lower()
    if text in ("on", "enabled", "poweron", "running"):
        return "on"
    if text in ("off", "disabled", "poweroff", "stopped"):
        return "off"
    return "unknown"


def _normalize_power(
    protocol: str,
    raw_power: Any,
    profile: dict,
    device_id: str,
) -> tuple[dict, list[dict]]:
    errors: list[dict] = []
    path = "power"
    value: Any = None
    label: Any = None
    if protocol == "ipmi":
        path = "chassis_status.power_byte"
        if not isinstance(raw_power, dict) or not isinstance(raw_power.get("byte"), int):
            errors.append(_unparsable(protocol, path, raw_power, "missing IPMI power byte"))
            return {"value": "unknown", "parsable": False, "raw": raw_snippet(raw_power)}, errors
        value = "on" if raw_power["byte"] & 0x01 else "off"
        label = value
    elif protocol == "snmp":
        path = "SNMP power OID"
        if isinstance(raw_power, dict) and isinstance(raw_power.get("text"), str):
            label = raw_power["text"]
            value = map_power_label(label)
        else:
            errors.append(_unparsable(protocol, path, raw_power, "missing SNMP power string"))
            return {"value": "unknown", "parsable": False, "raw": raw_snippet(raw_power)}, errors
    elif protocol == "redfish":
        path = "/PowerControl/0/PowerState"
        document = raw_power.get("document") if isinstance(raw_power, dict) else None
        if not isinstance(document, dict):
            errors.append(_unparsable(protocol, path, raw_power, "missing Redfish Power document"))
            return {"value": "unknown", "parsable": False, "raw": raw_snippet(raw_power)}, errors
        controls = document.get("PowerControl")
        if not isinstance(controls, list) or not controls or not isinstance(controls[0], dict):
            errors.append(
                _unparsable(protocol, path, controls, "PowerControl is not a non-empty array")
            )
            return {"value": "unknown", "parsable": False, "raw": raw_snippet(raw_power)}, errors
        field = controls[0].get("PowerState")
        expected_shape = profile.get("power_state_shape")
        if expected_shape == "string":
            if not isinstance(field, str):
                errors.append(
                    _unparsable(
                        protocol,
                        path,
                        field,
                        f"firmware expects string PowerState, got {type(field).__name__}",
                    )
                )
                return {"value": "unknown", "parsable": False, "raw": raw_snippet(field)}, errors
            label = field
        elif expected_shape == "object":
            if not isinstance(field, dict):
                errors.append(
                    _unparsable(
                        protocol,
                        path,
                        field,
                        f"firmware expects object PowerState, got {type(field).__name__}",
                    )
                )
                return {"value": "unknown", "parsable": False, "raw": raw_snippet(field)}, errors
            for key in ("State", "Value", "PowerState"):
                if isinstance(field.get(key), str):
                    label = field[key]
                    break
            if label is None and isinstance(field.get("Status"), dict):
                status_state = field["Status"].get("State")
                if isinstance(status_state, str):
                    label = status_state
            if label is None:
                errors.append(
                    _unparsable(
                        protocol,
                        path,
                        field,
                        "object PowerState has no recognized string value",
                    )
                )
                return {"value": "unknown", "parsable": False, "raw": raw_snippet(field)}, errors
        else:
            errors.append(
                _unparsable(protocol, path, field, "profile does not expose Redfish power state")
            )
            return {"value": "unknown", "parsable": False, "raw": raw_snippet(field)}, errors
        value = map_power_label(label)
    else:
        raise ValueError(f"unknown protocol {protocol}")
    if value not in VALID_POWER:
        value = "unknown"
    return {"value": value, "parsable": True, "raw": raw_snippet(label)}, errors


def _thresholds_for(kind: str, thresholds: dict) -> tuple[float, float]:
    if kind == "temperature":
        block = thresholds["temperature_c"]
    else:
        block = thresholds["fan_rpm"]
    return float(block["lower"]), float(block["upper"])


def _convert_temperature(value: float, unit: str) -> float:
    normalized_unit = unit.strip().upper().replace("°", "")
    if normalized_unit in ("F", "DEGREES F", "DEGF"):
        return (value - 32.0) * 5.0 / 9.0
    if normalized_unit in ("K", "KELVIN"):
        return value - 273.15
    return value


def _normalize_sensor_entry(
    protocol: str,
    entry: Any,
    index: int,
    thresholds: dict,
    device_id: str,
) -> tuple[dict | None, list[dict]]:
    errors: list[dict] = []
    if not isinstance(entry, dict):
        errors.append(
            _unparsable(protocol, f"sensors/{index}", entry, "sensor entry is not an object")
        )
        return None, errors
    name = entry.get("name")
    kind = entry.get("kind")
    if not isinstance(name, str) or not name:
        errors.append(
            _unparsable(protocol, f"sensors/{index}/name", name, "sensor name is missing")
        )
        return None, errors
    if kind not in ("temperature", "fan"):
        errors.append(
            _unparsable(protocol, f"sensors/{index}/kind", kind, "unsupported sensor kind")
        )
        return None, errors
    path = f"sensors/{name}/reading"
    raw_value = entry.get("reading", entry.get("value"))
    if isinstance(raw_value, bool) or not isinstance(raw_value, (int, float)):
        errors.append(
            _unparsable(
                protocol,
                path,
                raw_value,
                f"sensor reading is not numeric ({type(raw_value).__name__})",
            )
        )
        return None, errors
    if not math.isfinite(float(raw_value)):
        errors.append(_unparsable(protocol, path, raw_value, "sensor reading is not finite"))
        return None, errors
    value = float(raw_value)
    unit = str(entry.get("unit") or ("C" if kind == "temperature" else "RPM"))
    if kind == "temperature":
        value = _convert_temperature(value, unit)
        value = round(value, 3)
        unit = "C"
    else:
        value = int(round(value))
        unit = "RPM"
    lower, upper = _thresholds_for(kind, thresholds)
    return (
        {
            "name": name,
            "kind": kind,
            "value": value,
            "unit": unit,
            "lower": lower,
            "upper": upper,
            "parsable": True,
            "raw": raw_snippet(entry.get("raw_entry", raw_value)),
        },
        errors,
    )


def normalize_protocol_payload(
    protocol: str,
    payload: dict,
    *,
    device_id: str,
    profile: dict,
    thresholds: dict,
) -> dict:
    errors: list[dict] = []
    asset_raw = payload.get("asset") if isinstance(payload, dict) else None
    asset = {}
    if not isinstance(asset_raw, dict):
        errors.append(_unparsable(protocol, "asset", asset_raw, "asset is not an object"))
        asset_raw = {}
    for key in ("manufacturer", "model", "serial", "firmware_version"):
        value = asset_raw.get(key)
        if not isinstance(value, str):
            errors.append(
                _unparsable(protocol, f"asset.{key}", value, "asset field is not a string")
            )
            value = ""
        asset[key] = value
    power, power_errors = _normalize_power(
        protocol, payload.get("power_raw"), profile, device_id
    )
    errors.extend(power_errors)
    sensors = []
    for index, entry in enumerate(payload.get("sensors", []) or []):
        sensor, sensor_errors = _normalize_sensor_entry(
            protocol, entry, index, thresholds, device_id
        )
        errors.extend(sensor_errors)
        if sensor is not None:
            sensors.append(sensor)
    sensors.sort(key=lambda item: item["name"])
    usable = power["parsable"] or bool(sensors)
    return {
        "protocol": protocol,
        "success": True,
        "usable": usable,
        "asset": asset,
        "power": power,
        "sensors": sensors,
        "unparsable": errors,
        "raw_response": payload.get("raw", payload),
    }


def protocol_failure(protocol: str, message: str, raw: Any = None) -> dict:
    return {
        "protocol": protocol,
        "success": False,
        "usable": False,
        "asset": {},
        "power": {"value": "unknown", "parsable": False, "raw": raw_snippet(raw)},
        "sensors": [],
        "unparsable": [],
        "raw_response": raw if raw is not None else {},
        "error": message,
    }


def _canonical_sensor_value(item: dict):
    if item["kind"] == "temperature":
        return round(float(item["value"]), 3)
    return int(item["value"])


def compare_ipmi_redfish(device_id: str, protocol_results: dict[str, dict]) -> list[dict]:
    ipmi = protocol_results.get("ipmi")
    redfish = protocol_results.get("redfish")
    if not ipmi or not redfish or not ipmi.get("success") or not redfish.get("success"):
        return []
    issues = []
    if ipmi["power"]["parsable"] and redfish["power"]["parsable"]:
        if ipmi["power"]["value"] != redfish["power"]["value"]:
            issues.append(
                {
                    "device_id": device_id,
                    "item": "power_state",
                    "kind": "power",
                    "ipmi": {
                        "value": ipmi["power"]["value"],
                        "raw": ipmi["power"]["raw"],
                    },
                    "redfish": {
                        "value": redfish["power"]["value"],
                        "raw": redfish["power"]["raw"],
                    },
                }
            )
    ipmi_sensors = {
        item["name"]: item for item in ipmi.get("sensors", []) if item.get("parsable")
    }
    redfish_sensors = {
        item["name"]: item for item in redfish.get("sensors", []) if item.get("parsable")
    }
    for name in sorted(set(ipmi_sensors) & set(redfish_sensors)):
        left = ipmi_sensors[name]
        right = redfish_sensors[name]
        left_value = _canonical_sensor_value(left)
        right_value = _canonical_sensor_value(right)
        if left_value != right_value:
            issues.append(
                {
                    "device_id": device_id,
                    "item": f"sensor:{name}",
                    "kind": left["kind"],
                    "ipmi": {"value": left_value, "unit": left["unit"], "raw": left["raw"]},
                    "redfish": {
                        "value": right_value,
                        "unit": right["unit"],
                        "raw": right["raw"],
                    },
                }
            )
    return issues


def aggregate_device(
    *,
    device: dict,
    profile: dict,
    protocol_results: dict[str, dict],
    protocol_order: list[str] | None = None,
) -> dict:
    requested_order = protocol_order or profile["protocols"]
    order = [name for name in requested_order if name in protocol_results]
    successful = [name for name in order if protocol_results[name].get("success")]
    usable = [name for name in order if protocol_results[name].get("usable")]
    primary = usable[0] if usable else None
    failures = [name for name in order if not protocol_results[name].get("success")]
    unparsable = []
    for name in order:
        for item in protocol_results[name].get("unparsable", []):
            located = dict(item)
            located.setdefault("device_id", device["device_id"])
            unparsable.append(located)
    asset = dict(protocol_results[primary]["asset"]) if primary else {
        "manufacturer": "",
        "model": "",
        "serial": "",
        "firmware_version": "",
    }
    power_sources = [
        name
        for name in order
        if protocol_results[name].get("success")
        and protocol_results[name].get("power", {}).get("parsable")
    ]
    if power_sources:
        power_source = power_sources[0]
        power = {
            "value": protocol_results[power_source]["power"]["value"],
            "parsable": True,
            "source_protocol": power_source,
            "raw": protocol_results[power_source]["power"]["raw"],
        }
    else:
        power = {
            "value": "unknown",
            "parsable": False,
            "source_protocol": None,
            "raw": "",
        }
    sensor_map: dict[str, dict] = {}
    for name in order:
        result = protocol_results[name]
        if not result.get("success"):
            continue
        for sensor in result.get("sensors", []):
            if sensor["name"] not in sensor_map:
                chosen = dict(sensor)
                chosen["source_protocol"] = name
                sensor_map[sensor["name"]] = chosen
    sensors = [sensor_map[name] for name in sorted(sensor_map)]
    inconsistencies = compare_ipmi_redfish(device["device_id"], protocol_results)
    fallback_used = bool(failures and usable)
    if primary is not None and primary != order[0]:
        fallback_used = True
    if order and usable and order[0] in protocol_results:
        first = order[0]
        if power_sources and power_sources[0] != first:
            fallback_used = True
        if any(
            sensor.get("source_protocol") not in (None, first) for sensor in sensors
        ):
            fallback_used = True
    if not successful:
        status = "failed"
    elif not usable:
        status = "degraded"
    elif failures or unparsable or inconsistencies or fallback_used:
        status = "degraded"
    else:
        status = "ok"
    return {
        "device_id": device["device_id"],
        "rack_id": device["rack_id"],
        "firmware_profile": device["firmware_profile"],
        "firmware_version": device["firmware_version"],
        "status": status,
        "primary_protocol": primary,
        "protocols_used": successful,
        "fallback_used": fallback_used,
        "asset": asset,
        "power": power,
        "sensors": sensors,
        "unparsable_count": len(unparsable),
        "unparsable": unparsable,
        "inconsistencies": inconsistencies,
        "protocols": protocol_results,
    }
