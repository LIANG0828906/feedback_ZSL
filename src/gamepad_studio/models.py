"""Typed, serializable sample and replay data models."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Iterable

from .errors import ALGORITHM_VERSION, ParameterError, SchemaError
from .hashing import canonical_json, content_hash


def _require_mapping(data: Any, field_name: str) -> dict[str, Any]:
    if not isinstance(data, dict):
        raise SchemaError(f"{field_name} must be an object", field=field_name)
    return data


def _require(data: dict[str, Any], key: str, field_name: str) -> Any:
    if key not in data:
        raise SchemaError(f"missing required field {field_name}", field=field_name)
    return data[key]


def _require_str(data: dict[str, Any], key: str, field_name: str) -> str:
    value = _require(data, key, field_name)
    if not isinstance(value, str) or not value:
        raise SchemaError(f"{field_name} must be a non-empty string", field=field_name)
    return value


def _require_number(data: dict[str, Any], key: str, field_name: str) -> float:
    value = _require(data, key, field_name)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise SchemaError(f"{field_name} must be numeric", field=field_name)
    return float(value)


def _require_bool(data: dict[str, Any], key: str, field_name: str) -> bool:
    value = _require(data, key, field_name)
    if not isinstance(value, bool):
        raise SchemaError(f"{field_name} must be boolean", field=field_name)
    return value


@dataclass(frozen=True)
class CurveSpec:
    kind: str = "linear"
    exponent: float = 1.0
    points: tuple[tuple[float, float], ...] = ()

    @classmethod
    def from_dict(cls, data: Any, field_name: str = "curve") -> "CurveSpec":
        if data is None:
            return cls()
        obj = _require_mapping(data, field_name)
        kind = obj.get("kind", "linear")
        if kind not in {"linear", "power", "piecewise"}:
            raise SchemaError(
                f"{field_name}.kind must be linear, power, or piecewise",
                field=f"{field_name}.kind",
            )
        exponent = obj.get("exponent", 1.0)
        if isinstance(exponent, bool) or not isinstance(exponent, (int, float)):
            raise SchemaError(
                f"{field_name}.exponent must be numeric",
                field=f"{field_name}.exponent",
            )
        points_raw = obj.get("points", [])
        if not isinstance(points_raw, list):
            raise SchemaError(
                f"{field_name}.points must be a list",
                field=f"{field_name}.points",
            )
        points: list[tuple[float, float]] = []
        for index, point in enumerate(points_raw):
            if (
                not isinstance(point, (list, tuple))
                or len(point) != 2
                or any(isinstance(v, bool) or not isinstance(v, (int, float)) for v in point)
            ):
                raise SchemaError(
                    f"{field_name}.points[{index}] must be [x, y]",
                    field=f"{field_name}.points[{index}]",
                )
            points.append((float(point[0]), float(point[1])))
        return cls(kind=kind, exponent=float(exponent), points=tuple(points))

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "exponent": self.exponent,
            "points": [list(point) for point in self.points],
        }


@dataclass(frozen=True)
class AxisSpec:
    id: str
    kind: str
    minimum: float
    maximum: float
    deadzone: float = 0.0
    inverted: bool = False
    trigger_direction: str = "increasing"
    curve: CurveSpec = field(default_factory=CurveSpec)

    @property
    def bipolar(self) -> bool:
        return self.minimum < 0.0 < self.maximum

    @classmethod
    def from_dict(cls, data: Any, field_name: str = "axis") -> "AxisSpec":
        obj = _require_mapping(data, field_name)
        kind = _require_str(obj, "kind", f"{field_name}.kind")
        if kind not in {"stick", "trigger", "gyro"}:
            raise SchemaError(
                f"{field_name}.kind must be stick, trigger, or gyro",
                field=f"{field_name}.kind",
            )
        minimum = _require_number(obj, "minimum", f"{field_name}.minimum")
        maximum = _require_number(obj, "maximum", f"{field_name}.maximum")
        if minimum >= maximum:
            raise ParameterError(
                f"{field_name}.minimum must be smaller than maximum",
                field=f"{field_name}.minimum",
            )
        deadzone = obj.get("deadzone", 0.0)
        if isinstance(deadzone, bool) or not isinstance(deadzone, (int, float)):
            raise SchemaError(
                f"{field_name}.deadzone must be numeric",
                field=f"{field_name}.deadzone",
            )
        inverted = obj.get("inverted", False)
        if not isinstance(inverted, bool):
            raise SchemaError(
                f"{field_name}.inverted must be boolean",
                field=f"{field_name}.inverted",
            )
        direction = obj.get("trigger_direction", "increasing")
        if direction not in {"increasing", "decreasing"}:
            raise SchemaError(
                f"{field_name}.trigger_direction must be increasing or decreasing",
                field=f"{field_name}.trigger_direction",
            )
        return cls(
            id=_require_str(obj, "id", f"{field_name}.id"),
            kind=kind,
            minimum=minimum,
            maximum=maximum,
            deadzone=float(deadzone),
            inverted=inverted,
            trigger_direction=direction,
            curve=CurveSpec.from_dict(obj.get("curve"), f"{field_name}.curve"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "kind": self.kind,
            "minimum": self.minimum,
            "maximum": self.maximum,
            "deadzone": self.deadzone,
            "inverted": self.inverted,
            "trigger_direction": self.trigger_direction,
            "curve": self.curve.to_dict(),
        }


@dataclass(frozen=True)
class DeviceSpec:
    schema_version: int
    id: str
    name: str
    axes: tuple[AxisSpec, ...]
    buttons: tuple[str, ...]

    @classmethod
    def from_dict(cls, data: Any) -> "DeviceSpec":
        obj = _require_mapping(data, "device")
        schema_version = _require(obj, "schema_version", "device.schema_version")
        if not isinstance(schema_version, int) or isinstance(schema_version, bool):
            raise SchemaError(
                "device.schema_version must be an integer",
                field="device.schema_version",
            )
        axes_raw = _require(obj, "axes", "device.axes")
        buttons_raw = _require(obj, "buttons", "device.buttons")
        if not isinstance(axes_raw, list):
            raise SchemaError("device.axes must be a list", field="device.axes")
        if not isinstance(buttons_raw, list) or any(
            not isinstance(button, str) for button in buttons_raw
        ):
            raise SchemaError("device.buttons must be a list of strings", field="device.buttons")
        axes = tuple(
            AxisSpec.from_dict(item, f"device.axes[{index}]")
            for index, item in enumerate(axes_raw)
        )
        return cls(
            schema_version=schema_version,
            id=_require_str(obj, "id", "device.id"),
            name=_require_str(obj, "name", "device.name"),
            axes=axes,
            buttons=tuple(buttons_raw),
        )

    @property
    def axis_map(self) -> dict[str, AxisSpec]:
        return {axis.id: axis for axis in self.axes}

    @property
    def axis_ids(self) -> tuple[str, ...]:
        return tuple(axis.id for axis in self.axes)

    def structural_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "id": self.id,
            "name": self.name,
            "axes": [
                {
                    "id": axis.id,
                    "kind": axis.kind,
                    "minimum": axis.minimum,
                    "maximum": axis.maximum,
                }
                for axis in self.axes
            ],
            "buttons": list(self.buttons),
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "id": self.id,
            "name": self.name,
            "axes": [axis.to_dict() for axis in self.axes],
            "buttons": list(self.buttons),
        }

    def with_axis(self, axis_id: str, **changes: Any) -> "DeviceSpec":
        found = False
        axes: list[AxisSpec] = []
        for axis in self.axes:
            if axis.id == axis_id:
                axes.append(replace(axis, **changes))
                found = True
            else:
                axes.append(axis)
        if not found:
            raise ParameterError(
                f"axis {axis_id!r} is not present on device {self.id!r}",
                field="axis",
                context={"device_id": self.id, "axis_id": axis_id},
            )
        return replace(self, axes=tuple(axes))


@dataclass(frozen=True)
class BindingRule:
    id: str
    action: str
    kind: str
    buttons: tuple[str, ...] = ()
    source_axis: str | None = None
    window_ms: int = 0
    hold_ms: int = 0
    curve: CurveSpec | None = None
    enabled: bool = True

    @classmethod
    def from_dict(cls, data: Any, field_name: str = "rule") -> "BindingRule":
        obj = _require_mapping(data, field_name)
        kind = _require_str(obj, "kind", f"{field_name}.kind")
        if kind not in {"button", "combo", "long_press", "double_press", "axis"}:
            raise SchemaError(
                f"{field_name}.kind is not supported",
                field=f"{field_name}.kind",
            )
        buttons_raw = obj.get("buttons", [])
        if not isinstance(buttons_raw, list) or any(
            not isinstance(button, str) for button in buttons_raw
        ):
            raise SchemaError(
                f"{field_name}.buttons must be a list of strings",
                field=f"{field_name}.buttons",
            )
        source_axis = obj.get("source_axis")
        if source_axis is not None and not isinstance(source_axis, str):
            raise SchemaError(
                f"{field_name}.source_axis must be a string",
                field=f"{field_name}.source_axis",
            )
        window_ms = obj.get("window_ms", 0)
        hold_ms = obj.get("hold_ms", 0)
        for number, name in ((window_ms, "window_ms"), (hold_ms, "hold_ms")):
            if isinstance(number, bool) or not isinstance(number, int):
                raise SchemaError(
                    f"{field_name}.{name} must be an integer",
                    field=f"{field_name}.{name}",
                )
        enabled = obj.get("enabled", True)
        if not isinstance(enabled, bool):
            raise SchemaError(
                f"{field_name}.enabled must be boolean",
                field=f"{field_name}.enabled",
            )
        curve = CurveSpec.from_dict(obj.get("curve"), f"{field_name}.curve") if "curve" in obj else None
        return cls(
            id=_require_str(obj, "id", f"{field_name}.id"),
            action=_require_str(obj, "action", f"{field_name}.action"),
            kind=kind,
            buttons=tuple(buttons_raw),
            source_axis=source_axis,
            window_ms=window_ms,
            hold_ms=hold_ms,
            curve=curve,
            enabled=enabled,
        )

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {
            "id": self.id,
            "action": self.action,
            "kind": self.kind,
            "buttons": list(self.buttons),
            "window_ms": self.window_ms,
            "hold_ms": self.hold_ms,
            "enabled": self.enabled,
        }
        if self.source_axis is not None:
            data["source_axis"] = self.source_axis
        if self.curve is not None:
            data["curve"] = self.curve.to_dict()
        return data


@dataclass(frozen=True)
class BindingTable:
    schema_version: int
    rules: tuple[BindingRule, ...]
    declared_version: str | None = None

    @classmethod
    def from_dict(cls, data: Any) -> "BindingTable":
        obj = _require_mapping(data, "bindings")
        schema_version = _require(obj, "schema_version", "bindings.schema_version")
        if not isinstance(schema_version, int) or isinstance(schema_version, bool):
            raise SchemaError(
                "bindings.schema_version must be an integer",
                field="bindings.schema_version",
            )
        rules_raw = _require(obj, "rules", "bindings.rules")
        if not isinstance(rules_raw, list):
            raise SchemaError("bindings.rules must be a list", field="bindings.rules")
        rules = tuple(
            BindingRule.from_dict(item, f"bindings.rules[{index}]")
            for index, item in enumerate(rules_raw)
        )
        declared_version = obj.get("version")
        if declared_version is not None and not isinstance(declared_version, str):
            raise SchemaError(
                "bindings.version must be a string",
                field="bindings.version",
            )
        return cls(
            schema_version=schema_version,
            rules=rules,
            declared_version=declared_version,
        )

    def to_dict(self) -> dict[str, Any]:
        data = {
            "schema_version": self.schema_version,
            "rules": [rule.to_dict() for rule in self.rules],
        }
        if self.declared_version is not None:
            data["version"] = self.declared_version
        return data

    @property
    def content_version(self) -> str:
        return content_hash(
            {
                "schema_version": self.schema_version,
                "rules": [rule.to_dict() for rule in self.rules],
            }
        )

    def rule_map(self) -> dict[str, BindingRule]:
        return {rule.id: rule for rule in self.rules}

    def with_rule(self, rule_id: str, **changes: Any) -> "BindingTable":
        found = False
        rules: list[BindingRule] = []
        for rule in self.rules:
            if rule.id == rule_id:
                rules.append(replace(rule, **changes))
                found = True
            else:
                rules.append(rule)
        if not found:
            raise ParameterError(
                f"binding rule {rule_id!r} does not exist",
                field="rule_id",
                context={"rule_id": rule_id},
            )
        return replace(self, rules=tuple(rules))


@dataclass(frozen=True)
class InputEvent:
    seq: int
    t_ms: int
    type: str
    axis: str | None = None
    value: float | None = None
    button: str | None = None
    pressed: bool | None = None
    repeat: bool = False

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {"seq": self.seq, "t_ms": self.t_ms, "type": self.type}
        if self.type == "axis":
            data.update({"axis": self.axis, "value": self.value})
        else:
            data.update(
                {"button": self.button, "pressed": self.pressed, "repeat": self.repeat}
            )
        return data


@dataclass(frozen=True)
class AxisSample:
    t_ms: int
    axis: str
    minimum: float
    maximum: float

    @classmethod
    def from_dict(cls, data: Any, field_name: str) -> "AxisSample":
        obj = _require_mapping(data, field_name)
        return cls(
            t_ms=int(_require(obj, "t_ms", f"{field_name}.t_ms")),
            axis=_require_str(obj, "axis", f"{field_name}.axis"),
            minimum=_require_number(obj, "min", f"{field_name}.min"),
            maximum=_require_number(obj, "max", f"{field_name}.max"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "t_ms": self.t_ms,
            "axis": self.axis,
            "min": self.minimum,
            "max": self.maximum,
        }


@dataclass(frozen=True)
class ActionSample:
    t_ms: int
    action: str
    pressed: bool | None = None
    trigger_count: int | None = None
    repeat_count: int | None = None
    reason: str | None = None

    @classmethod
    def from_dict(cls, data: Any, field_name: str) -> "ActionSample":
        obj = _require_mapping(data, field_name)
        pressed = obj.get("pressed")
        if pressed is not None and not isinstance(pressed, bool):
            raise SchemaError(f"{field_name}.pressed must be boolean", field=f"{field_name}.pressed")
        for key in ("trigger_count", "repeat_count"):
            value = obj.get(key)
            if value is not None and (
                isinstance(value, bool) or not isinstance(value, int)
            ):
                raise SchemaError(f"{field_name}.{key} must be an integer", field=f"{field_name}.{key}")
        reason = obj.get("reason")
        if reason is not None and not isinstance(reason, str):
            raise SchemaError(f"{field_name}.reason must be a string", field=f"{field_name}.reason")
        return cls(
            t_ms=int(_require(obj, "t_ms", f"{field_name}.t_ms")),
            action=_require_str(obj, "action", f"{field_name}.action"),
            pressed=pressed,
            trigger_count=obj.get("trigger_count"),
            repeat_count=obj.get("repeat_count"),
            reason=reason,
        )

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {"t_ms": self.t_ms, "action": self.action}
        if self.pressed is not None:
            data["pressed"] = self.pressed
        if self.trigger_count is not None:
            data["trigger_count"] = self.trigger_count
        if self.repeat_count is not None:
            data["repeat_count"] = self.repeat_count
        if self.reason is not None:
            data["reason"] = self.reason
        return data


@dataclass(frozen=True)
class TriggerSample:
    action: str
    t_ms: int
    source_event_seq: int
    trigger_count: int
    repeat: bool = False

    @classmethod
    def from_dict(cls, data: Any, field_name: str) -> "TriggerSample":
        obj = _require_mapping(data, field_name)
        return cls(
            action=_require_str(obj, "action", f"{field_name}.action"),
            t_ms=int(_require(obj, "t_ms", f"{field_name}.t_ms")),
            source_event_seq=int(
                _require(obj, "source_event_seq", f"{field_name}.source_event_seq")
            ),
            trigger_count=int(
                _require(obj, "trigger_count", f"{field_name}.trigger_count")
            ),
            repeat=bool(obj.get("repeat", False)),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "action": self.action,
            "t_ms": self.t_ms,
            "source_event_seq": self.source_event_seq,
            "trigger_count": self.trigger_count,
            "repeat": self.repeat,
        }


@dataclass(frozen=True)
class TruthManifest:
    schema_version: int
    device_id: str
    algorithm_version: str
    binding_version: str
    axis_tolerance: float
    time_tolerance_ms: int
    axis_samples: tuple[AxisSample, ...]
    action_samples: tuple[ActionSample, ...]
    trigger_samples: tuple[TriggerSample, ...]

    @classmethod
    def from_dict(cls, data: Any) -> "TruthManifest":
        obj = _require_mapping(data, "truth")
        schema_version = _require(obj, "schema_version", "truth.schema_version")
        axis_samples_raw = _require(obj, "axis_samples", "truth.axis_samples")
        action_samples_raw = _require(obj, "action_samples", "truth.action_samples")
        trigger_samples_raw = _require(obj, "trigger_samples", "truth.trigger_samples")
        for name, value in (
            ("axis_samples", axis_samples_raw),
            ("action_samples", action_samples_raw),
            ("trigger_samples", trigger_samples_raw),
        ):
            if not isinstance(value, list):
                raise SchemaError(f"truth.{name} must be a list", field=f"truth.{name}")
        return cls(
            schema_version=int(schema_version),
            device_id=_require_str(obj, "device_id", "truth.device_id"),
            algorithm_version=_require_str(
                obj, "algorithm_version", "truth.algorithm_version"
            ),
            binding_version=_require_str(
                obj, "binding_version", "truth.binding_version"
            ),
            axis_tolerance=float(obj.get("axis_tolerance", 0.01)),
            time_tolerance_ms=int(obj.get("time_tolerance_ms", 0)),
            axis_samples=tuple(
                AxisSample.from_dict(item, f"truth.axis_samples[{index}]")
                for index, item in enumerate(axis_samples_raw)
            ),
            action_samples=tuple(
                ActionSample.from_dict(item, f"truth.action_samples[{index}]")
                for index, item in enumerate(action_samples_raw)
            ),
            trigger_samples=tuple(
                TriggerSample.from_dict(item, f"truth.trigger_samples[{index}]")
                for index, item in enumerate(trigger_samples_raw)
            ),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "device_id": self.device_id,
            "algorithm_version": self.algorithm_version,
            "binding_version": self.binding_version,
            "axis_tolerance": self.axis_tolerance,
            "time_tolerance_ms": self.time_tolerance_ms,
            "axis_samples": [sample.to_dict() for sample in self.axis_samples],
            "action_samples": [sample.to_dict() for sample in self.action_samples],
            "trigger_samples": [sample.to_dict() for sample in self.trigger_samples],
        }


@dataclass(frozen=True)
class ActionSnapshot:
    action: str
    pressed: bool = False
    active: bool = False
    value: float = 0.0
    trigger_count: int = 0
    repeat_count: int = 0
    trigger_t_ms: int | None = None
    trigger_seq: int | None = None
    last_reason: str = "idle"
    triggered_now: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "action": self.action,
            "pressed": self.pressed,
            "active": self.active,
            "value": self.value,
            "trigger_count": self.trigger_count,
            "repeat_count": self.repeat_count,
            "trigger_t_ms": self.trigger_t_ms,
            "trigger_seq": self.trigger_seq,
            "last_reason": self.last_reason,
            "triggered_now": self.triggered_now,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ActionSnapshot":
        return cls(
            action=str(data["action"]),
            pressed=bool(data["pressed"]),
            active=bool(data["active"]),
            value=float(data["value"]),
            trigger_count=int(data["trigger_count"]),
            repeat_count=int(data["repeat_count"]),
            trigger_t_ms=(
                int(data["trigger_t_ms"]) if data.get("trigger_t_ms") is not None else None
            ),
            trigger_seq=(
                int(data["trigger_seq"]) if data.get("trigger_seq") is not None else None
            ),
            last_reason=str(data["last_reason"]),
            triggered_now=bool(data["triggered_now"]),
        )


@dataclass(frozen=True)
class AxisFrame:
    seq: int
    t_ms: int
    axis: str
    raw: float
    oriented: float
    normalized: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "seq": self.seq,
            "t_ms": self.t_ms,
            "axis": self.axis,
            "raw": self.raw,
            "oriented": self.oriented,
            "normalized": self.normalized,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "AxisFrame":
        return cls(
            seq=int(data["seq"]),
            t_ms=int(data["t_ms"]),
            axis=str(data["axis"]),
            raw=float(data["raw"]),
            oriented=float(data["oriented"]),
            normalized=float(data["normalized"]),
        )


@dataclass(frozen=True)
class SampleBundle:
    device: DeviceSpec
    bindings: BindingTable
    events: tuple[InputEvent, ...]
    truth: TruthManifest
    source_path: Path | None = None

    @property
    def input_hash(self) -> str:
        return content_hash(
            {
                "device_id": self.device.id,
                "events": [event.to_dict() for event in self.events],
            }
        )

    @property
    def binding_version(self) -> str:
        return self.bindings.content_version

    @property
    def algorithm_version(self) -> str:
        return ALGORITHM_VERSION

    def event_map(self) -> dict[int, InputEvent]:
        return {event.seq: event for event in self.events}

    def axis_events(self, axis_id: str) -> tuple[InputEvent, ...]:
        return tuple(
            event
            for event in self.events
            if event.type == "axis" and event.axis == axis_id
        )

    def metadata(self) -> dict[str, Any]:
        return {
            "sample_path": str(self.source_path) if self.source_path else None,
            "device_id": self.device.id,
            "device_name": self.device.name,
            "input_hash": self.input_hash,
            "binding_version": self.binding_version,
            "algorithm_version": self.algorithm_version,
            "event_count": len(self.events),
            "axis_count": len(self.device.axes),
            "button_count": len(self.device.buttons),
            "rule_count": len(self.bindings.rules),
        }


def events_canonical(events: Iterable[InputEvent]) -> str:
    return canonical_json([event.to_dict() for event in events])


__all__ = [
    "ActionSample",
    "ActionSnapshot",
    "AxisFrame",
    "AxisSample",
    "AxisSpec",
    "BindingRule",
    "BindingTable",
    "CurveSpec",
    "DeviceSpec",
    "InputEvent",
    "SampleBundle",
    "TriggerSample",
    "TruthManifest",
    "events_canonical",
]

