"""Sample loading and structural validation."""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

from .curves import validate_curve
from .errors import (
    ALGORITHM_VERSION,
    AxisError,
    Issue,
    OrderError,
    SampleValidationError,
    SchemaError,
    StudioError,
)
from .models import (
    BindingTable,
    DeviceSpec,
    InputEvent,
    SampleBundle,
    TruthManifest,
)

WINDOW_BOUNDS = {
    "combo": (1, 5_000),
    "double_press": (20, 5_000),
}
HOLD_BOUNDS = (20, 30_000)
DEADZONE_BOUNDS = (0.0, 0.95)


def _read_json(path: Path) -> Any:
    try:
        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError as exc:
        raise SchemaError(
            f"sample file is missing: {path.name}",
            field=path.name,
        ) from exc
    except json.JSONDecodeError as exc:
        raise SchemaError(
            f"invalid JSON in {path.name}: {exc.msg}",
            field=path.name,
            context={"line": exc.lineno, "column": exc.colno},
        ) from exc
    except OSError as exc:
        raise SchemaError(
            f"cannot read {path.name}: {exc}",
            field=path.name,
        ) from exc


def _validate_device(device: DeviceSpec) -> list[Issue]:
    issues: list[Issue] = []
    seen_axes: dict[str, int] = {}
    for index, axis in enumerate(device.axes):
        field_name = f"device.axes[{index}].id"
        if axis.id in seen_axes:
            issues.append(
                Issue(
                    code="E_AXIS",
                    field=field_name,
                    event_seq=None,
                    message=f"duplicate axis id {axis.id!r}",
                    context={"axis": axis.id, "first_index": seen_axes[axis.id]},
                )
            )
        else:
            seen_axes[axis.id] = index
        if not DEADZONE_BOUNDS[0] <= axis.deadzone <= DEADZONE_BOUNDS[1]:
            issues.append(
                Issue(
                    code="E_PARA",
                    field=f"device.axes[{index}].deadzone",
                    event_seq=None,
                    message=f"deadzone must be within [{DEADZONE_BOUNDS[0]}, {DEADZONE_BOUNDS[1]}]",
                    context={"axis": axis.id, "deadzone": axis.deadzone},
                )
            )
        try:
            validate_curve(axis.curve, f"device.axes[{index}].curve")
        except StudioError as exc:
            issues.append(exc.to_issue())
        if axis.kind == "trigger" and axis.bipolar:
            issues.append(
                Issue(
                    code="E_PARA",
                    field=f"device.axes[{index}].kind",
                    event_seq=None,
                    message="trigger axes must be unipolar",
                    context={"axis": axis.id},
                )
            )
    seen_buttons: set[str] = set()
    for index, button in enumerate(device.buttons):
        if button in seen_buttons:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"device.buttons[{index}]",
                    event_seq=None,
                    message=f"duplicate button id {button!r}",
                    context={"button": button},
                )
            )
        seen_buttons.add(button)
    return issues


def _validate_bindings(device: DeviceSpec, bindings: BindingTable) -> list[Issue]:
    issues: list[Issue] = []
    axis_map = device.axis_map
    button_set = set(device.buttons)
    rule_ids: set[str] = set()
    action_ids: set[str] = set()
    for index, rule in enumerate(bindings.rules):
        prefix = f"bindings.rules[{index}]"
        if rule.id in rule_ids:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.id",
                    event_seq=None,
                    message=f"duplicate binding rule id {rule.id!r}",
                    context={"rule_id": rule.id},
                )
            )
        rule_ids.add(rule.id)
        if rule.action in action_ids:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.action",
                    event_seq=None,
                    message=f"duplicate action id {rule.action!r}",
                    context={"action": rule.action},
                )
            )
        action_ids.add(rule.action)

        for button_index, button in enumerate(rule.buttons):
            if button not in button_set:
                issues.append(
                    Issue(
                        code="E_AXIS",
                        field=f"{prefix}.buttons[{button_index}]",
                        event_seq=None,
                        message=f"button {button!r} is not declared by device {device.id!r}",
                        context={"button": button, "device_id": device.id},
                    )
                )
        if len(set(rule.buttons)) != len(rule.buttons):
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.buttons",
                    event_seq=None,
                    message="binding buttons must be unique",
                    context={"rule_id": rule.id},
                )
            )

        if rule.kind == "button" and len(rule.buttons) != 1:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.buttons",
                    event_seq=None,
                    message="button bindings require exactly one button",
                    context={"rule_id": rule.id},
                )
            )
        if rule.kind == "combo" and len(rule.buttons) < 2:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.buttons",
                    event_seq=None,
                    message="combo bindings require at least two buttons",
                    context={"rule_id": rule.id},
                )
            )
        if rule.kind in {"long_press", "double_press"} and len(rule.buttons) != 1:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.buttons",
                    event_seq=None,
                    message=f"{rule.kind} bindings require exactly one button",
                    context={"rule_id": rule.id},
                )
            )
        if rule.kind == "axis":
            if not rule.source_axis:
                issues.append(
                    Issue(
                        code="E_SCHEMA",
                        field=f"{prefix}.source_axis",
                        event_seq=None,
                        message="axis bindings require source_axis",
                        context={"rule_id": rule.id},
                    )
                )
            elif rule.source_axis not in axis_map:
                issues.append(
                    Issue(
                        code="E_AXIS",
                        field=f"{prefix}.source_axis",
                        event_seq=None,
                        message=f"axis {rule.source_axis!r} is not declared by device {device.id!r}",
                        context={"axis": rule.source_axis, "device_id": device.id},
                    )
                )
            if rule.buttons:
                issues.append(
                    Issue(
                        code="E_SCHEMA",
                        field=f"{prefix}.buttons",
                        event_seq=None,
                        message="axis bindings cannot reference buttons",
                        context={"rule_id": rule.id},
                    )
                )
        elif rule.source_axis is not None:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"{prefix}.source_axis",
                    event_seq=None,
                    message="source_axis is only valid for axis bindings",
                    context={"rule_id": rule.id},
                )
            )

        if rule.kind in WINDOW_BOUNDS:
            lower, upper = WINDOW_BOUNDS[rule.kind]
            if not lower <= rule.window_ms <= upper:
                issues.append(
                    Issue(
                        code="E_PARA",
                        field=f"{prefix}.window_ms",
                        event_seq=None,
                        message=f"window_ms must be within [{lower}, {upper}]",
                        context={"rule_id": rule.id, "window_ms": rule.window_ms},
                    )
                )
        elif rule.window_ms != 0:
            issues.append(
                Issue(
                    code="E_PARA",
                    field=f"{prefix}.window_ms",
                    event_seq=None,
                    message="window_ms is not used by this binding kind",
                    context={"rule_id": rule.id, "window_ms": rule.window_ms},
                )
            )
        if rule.kind == "long_press":
            if not HOLD_BOUNDS[0] <= rule.hold_ms <= HOLD_BOUNDS[1]:
                issues.append(
                    Issue(
                        code="E_PARA",
                        field=f"{prefix}.hold_ms",
                        event_seq=None,
                        message=f"hold_ms must be within [{HOLD_BOUNDS[0]}, {HOLD_BOUNDS[1]}]",
                        context={"rule_id": rule.id, "hold_ms": rule.hold_ms},
                    )
                )
        elif rule.hold_ms != 0:
            issues.append(
                Issue(
                    code="E_PARA",
                    field=f"{prefix}.hold_ms",
                    event_seq=None,
                    message="hold_ms is not used by this binding kind",
                    context={"rule_id": rule.id, "hold_ms": rule.hold_ms},
                )
            )
        if rule.curve is not None:
            try:
                validate_curve(rule.curve, f"{prefix}.curve")
            except StudioError as exc:
                issues.append(exc.to_issue())
    return issues


def _parse_events(
    raw: Any, device: DeviceSpec
) -> tuple[tuple[InputEvent, ...], list[Issue]]:
    if not isinstance(raw, dict):
        raise SchemaError("events must be an object", field="events")
    events_raw = raw.get("events")
    if events_raw is None:
        raise SchemaError("missing required field events.events", field="events.events")
    if not isinstance(events_raw, list):
        raise SchemaError("events.events must be a list", field="events.events")
    axis_map = device.axis_map
    button_set = set(device.buttons)
    events: list[InputEvent] = []
    previous_seq: int | None = None
    previous_t: int | None = None
    for index, item in enumerate(events_raw):
        prefix = f"events.events[{index}]"
        if not isinstance(item, dict):
            raise SchemaError(f"{prefix} must be an object", field=prefix)
        seq = item.get("seq")
        if isinstance(seq, bool) or not isinstance(seq, int):
            raise SchemaError(f"{prefix}.seq must be an integer", field=f"{prefix}.seq")
        t_ms = item.get("t_ms")
        if isinstance(t_ms, bool) or not isinstance(t_ms, int):
            raise SchemaError(
                f"{prefix}.t_ms must be an integer",
                field=f"{prefix}.t_ms",
                event_seq=seq,
            )
        if previous_seq is not None and seq <= previous_seq:
            raise OrderError(
                "event sequence numbers must strictly increase",
                field=f"{prefix}.seq",
                event_seq=seq,
            )
        if previous_t is not None and t_ms < previous_t:
            raise OrderError(
                f"logical timestamp moved backwards: {previous_t} -> {t_ms}",
                field=f"{prefix}.t_ms",
                event_seq=seq,
            )
        previous_seq = seq
        previous_t = t_ms
        event_type = item.get("type")
        if event_type not in {"axis", "button"}:
            raise SchemaError(
                f"{prefix}.type must be axis or button",
                field=f"{prefix}.type",
                event_seq=seq,
            )
        if event_type == "axis":
            axis_id = item.get("axis")
            if not isinstance(axis_id, str):
                raise SchemaError(
                    f"{prefix}.axis must be a string",
                    field=f"{prefix}.axis",
                    event_seq=seq,
                )
            if axis_id not in axis_map:
                raise AxisError(
                    f"event references undeclared axis {axis_id!r}",
                    field=f"{prefix}.axis",
                    event_seq=seq,
                    context={"axis": axis_id, "device_id": device.id},
                )
            value = item.get("value")
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise SchemaError(
                    f"{prefix}.value must be numeric",
                    field=f"{prefix}.value",
                    event_seq=seq,
                )
            numeric_value = float(value)
            axis = axis_map[axis_id]
            if not math.isfinite(numeric_value):
                raise AxisError(
                    f"axis {axis_id!r} value must be finite",
                    field=f"{prefix}.value",
                    event_seq=seq,
                    context={"axis": axis_id},
                )
            if not axis.minimum <= numeric_value <= axis.maximum:
                raise AxisError(
                    f"axis {axis_id!r} value {numeric_value} is outside "
                    f"[{axis.minimum}, {axis.maximum}]",
                    field=f"{prefix}.value",
                    event_seq=seq,
                    context={
                        "axis": axis_id,
                        "minimum": axis.minimum,
                        "maximum": axis.maximum,
                    },
                )
            events.append(
                InputEvent(
                    seq=seq,
                    t_ms=t_ms,
                    type="axis",
                    axis=axis_id,
                    value=numeric_value,
                )
            )
        else:
            button = item.get("button")
            if not isinstance(button, str):
                raise SchemaError(
                    f"{prefix}.button must be a string",
                    field=f"{prefix}.button",
                    event_seq=seq,
                )
            if button not in button_set:
                raise AxisError(
                    f"event references undeclared button {button!r}",
                    field=f"{prefix}.button",
                    event_seq=seq,
                    context={"button": button, "device_id": device.id},
                )
            pressed = item.get("pressed")
            if not isinstance(pressed, bool):
                raise SchemaError(
                    f"{prefix}.pressed must be boolean",
                    field=f"{prefix}.pressed",
                    event_seq=seq,
                )
            repeat = item.get("repeat", False)
            if not isinstance(repeat, bool):
                raise SchemaError(
                    f"{prefix}.repeat must be boolean",
                    field=f"{prefix}.repeat",
                    event_seq=seq,
                )
            if repeat and not pressed:
                raise SchemaError(
                    "auto-repeat events must be pressed=true",
                    field=f"{prefix}.repeat",
                    event_seq=seq,
                )
            events.append(
                InputEvent(
                    seq=seq,
                    t_ms=t_ms,
                    type="button",
                    button=button,
                    pressed=pressed,
                    repeat=repeat,
                )
            )
    return tuple(events), []


def _validate_truth(
    truth: TruthManifest,
    device: DeviceSpec,
    bindings: BindingTable,
) -> list[Issue]:
    issues: list[Issue] = []
    if truth.device_id != device.id:
        issues.append(
            Issue(
                code="E_SCHEMA",
                field="truth.device_id",
                message=f"truth device_id {truth.device_id!r} does not match {device.id!r}",
                context={"expected": device.id, "actual": truth.device_id},
            )
        )
    if truth.binding_version != bindings.content_version:
        issues.append(
            Issue(
                code="E_SCHEMA",
                field="truth.binding_version",
                message="truth binding_version does not match bindings content",
                context={
                    "expected": bindings.content_version,
                    "actual": truth.binding_version,
                },
            )
        )
    if truth.algorithm_version != ALGORITHM_VERSION:
        issues.append(
            Issue(
                code="E_SCHEMA",
                field="truth.algorithm_version",
                message="truth algorithm_version does not match the replay algorithm",
                context={"actual": truth.algorithm_version},
            )
        )
    if truth.axis_tolerance < 0.0 or truth.axis_tolerance > 1.0:
        issues.append(
            Issue(
                code="E_PARA",
                field="truth.axis_tolerance",
                message="axis_tolerance must be within [0, 1]",
            )
        )
    axis_ids = set(device.axis_ids)
    action_ids = {rule.action for rule in bindings.rules}
    for index, sample in enumerate(truth.axis_samples):
        if sample.axis not in axis_ids:
            issues.append(
                Issue(
                    code="E_AXIS",
                    field=f"truth.axis_samples[{index}].axis",
                    message=f"truth references unknown axis {sample.axis!r}",
                    context={"axis": sample.axis},
                )
            )
        if sample.minimum > sample.maximum:
            issues.append(
                Issue(
                    code="E_SCHEMA",
                    field=f"truth.axis_samples[{index}]",
                    message="axis sample min must not exceed max",
                )
            )
    for collection_name, collection in (
        ("action_samples", truth.action_samples),
        ("trigger_samples", truth.trigger_samples),
    ):
        for index, sample in enumerate(collection):
            if sample.action not in action_ids:
                issues.append(
                    Issue(
                        code="E_SCHEMA",
                        field=f"truth.{collection_name}[{index}].action",
                        message=f"truth references unknown action {sample.action!r}",
                        context={"action": sample.action},
                    )
                )
    return issues


def validate_bundle(bundle: SampleBundle) -> list[Issue]:
    issues: list[Issue] = []
    issues.extend(_validate_device(bundle.device))
    issues.extend(_validate_bindings(bundle.device, bundle.bindings))
    issues.extend(_validate_truth(bundle.truth, bundle.device, bundle.bindings))
    return issues


def validate_mapping(device: DeviceSpec, bindings: BindingTable) -> list[Issue]:
    """Validate only the mapping inputs, excluding stale truth metadata."""

    return _validate_device(device) + _validate_bindings(device, bindings)


def load_sample(path: str | Path, *, strict: bool = True) -> SampleBundle:
    sample_path = Path(path)
    if not sample_path.is_dir():
        raise SampleValidationError(
            [
                Issue(
                    code="E_SCHEMA",
                    field="sample_path",
                    message=f"sample directory does not exist: {sample_path}",
                )
            ]
        )
    try:
        device = DeviceSpec.from_dict(_read_json(sample_path / "device.json"))
        bindings = BindingTable.from_dict(_read_json(sample_path / "bindings.json"))
        events, issues = _parse_events(
            _read_json(sample_path / "events.json"),
            device,
        )
        truth = TruthManifest.from_dict(_read_json(sample_path / "truth.json"))
        bundle = SampleBundle(
            device=device,
            bindings=bindings,
            events=events,
            truth=truth,
            source_path=sample_path.resolve(),
        )
        issues = (
            _validate_device(device)
            + _validate_bindings(device, bindings)
            + issues
            + _validate_truth(truth, device, bindings)
        )
    except SampleValidationError:
        raise
    except StudioError as exc:
        raise SampleValidationError([exc.to_issue()]) from exc
    if issues and strict:
        raise SampleValidationError(issues)
    return bundle


def validate_sample_directory(path: str | Path) -> list[Issue]:
    try:
        load_sample(path, strict=True)
    except SampleValidationError as exc:
        return exc.issues
    return []


def write_sample_json(path: str | Path, payload: Any) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    temporary = target.with_suffix(target.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        handle.write(text)
        handle.flush()
    temporary.replace(target)


__all__ = [
    "DEADZONE_BOUNDS",
    "HOLD_BOUNDS",
    "WINDOW_BOUNDS",
    "load_sample",
    "validate_bundle",
    "validate_mapping",
    "validate_sample_directory",
    "write_sample_json",
]
