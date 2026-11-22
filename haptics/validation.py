from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .errors import ErrorCode, Issue
from .models import (
    ChannelCapability,
    DeviceCapabilities,
    EnvelopeTemplate,
    EventStream,
    HapticEvent,
)


@dataclass(frozen=True)
class ValidationResult:
    issues: tuple[Issue, ...] = ()

    @property
    def ok(self) -> bool:
        return not self.issues

    def by_code(self, code: ErrorCode) -> tuple[Issue, ...]:
        return tuple(issue for issue in self.issues if issue.code == code)

    def to_dict(self) -> dict[str, Any]:
        return {"ok": self.ok, "issues": [issue.to_dict() for issue in self.issues]}


def _number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def validate_capabilities(data: DeviceCapabilities | dict[str, Any]) -> ValidationResult:
    issues: list[Issue] = []
    if isinstance(data, DeviceCapabilities):
        capabilities = data
    else:
        capabilities = _capabilities_from_dict(data, issues)
        if capabilities is None:
            return ValidationResult(tuple(issues))

    if not isinstance(capabilities.device_id, str) or not capabilities.device_id:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                "device_id must be a non-empty string",
                event_id="<capability>",
                field="device_id",
            )
        )
    if not isinstance(capabilities.capability_version, int) or capabilities.capability_version < 1:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                "capability_version must be a positive integer",
                event_id="<capability>",
                field="capability_version",
            )
        )
    if not _number(capabilities.frame_ms) or capabilities.frame_ms <= 0:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                "frame_ms must be a positive finite number",
                event_id="<capability>",
                field="frame_ms",
            )
        )
    if not capabilities.channels:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                "at least one channel is required",
                event_id="<capability>",
                field="channels",
            )
        )

    seen_channels: set[int] = set()
    for channel_spec in capabilities.channels:
        if channel_spec.channel in seen_channels:
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "duplicate channel capability",
                    event_id="<capability>",
                    channel=channel_spec.channel,
                    field="channel",
                )
            )
        seen_channels.add(channel_spec.channel)
        if not isinstance(channel_spec.channel, int) or channel_spec.channel < 0:
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "channel must be a non-negative integer",
                    event_id="<capability>",
                    channel=channel_spec.channel,
                    field="channel",
                )
            )
        if not _number(channel_spec.min_event_interval_ms) or channel_spec.min_event_interval_ms < 0:
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "min_event_interval_ms must be a non-negative finite number",
                    event_id="<capability>",
                    channel=channel_spec.channel,
                    field="min_event_interval_ms",
                )
            )
        if (
            not isinstance(channel_spec.max_events_per_packet, int)
            or isinstance(channel_spec.max_events_per_packet, bool)
            or channel_spec.max_events_per_packet < 1
        ):
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "max_events_per_packet must be a positive integer",
                    event_id="<capability>",
                    channel=channel_spec.channel,
                    field="max_events_per_packet",
                )
            )
        window = channel_spec.packet_window_ms
        if (
            not isinstance(window, tuple)
            or len(window) != 2
            or not all(_number(item) for item in window)
            or window[0] < 0
            or window[1] <= window[0]
        ):
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "packet_window_ms must be [min, max] with 0 <= min < max",
                    event_id="<capability>",
                    channel=channel_spec.channel,
                    field="packet_window_ms",
                )
            )
        if not channel_spec.envelope_templates:
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "at least one envelope template is required",
                    event_id="<capability>",
                    channel=channel_spec.channel,
                    field="envelope_templates",
                )
            )
        for name, template in channel_spec.envelope_templates.items():
            if not isinstance(name, str) or not name:
                issues.append(
                    Issue(
                        ErrorCode.E_PARA,
                        "envelope template name must be non-empty",
                        event_id="<capability>",
                        channel=channel_spec.channel,
                        field="envelope_templates.name",
                    )
                )
            if not template.samples:
                issues.append(
                    Issue(
                        ErrorCode.E_PARA,
                        "envelope samples must not be empty",
                        event_id="<capability>",
                        channel=channel_spec.channel,
                        field=f"envelope_templates.{name}.samples",
                    )
                )
            for index, sample in enumerate(template.samples):
                if not _number(sample) or sample < 0 or sample > 1:
                    issues.append(
                        Issue(
                            ErrorCode.E_PARA,
                            "envelope sample must be finite and within [0, 1]",
                            event_id="<capability>",
                            channel=channel_spec.channel,
                            field=f"envelope_templates.{name}.samples[{index}]",
                            details={"value": sample},
                        )
                    )
            if not _number(template.sample_ms) or template.sample_ms <= 0:
                issues.append(
                    Issue(
                        ErrorCode.E_PARA,
                        "envelope sample_ms must be a positive finite number",
                        event_id="<capability>",
                        channel=channel_spec.channel,
                        field=f"envelope_templates.{name}.sample_ms",
                    )
                )
    return ValidationResult(tuple(issues))


def _capabilities_from_dict(
    data: dict[str, Any], issues: list[Issue]
) -> DeviceCapabilities | None:
    try:
        channels: list[ChannelCapability] = []
        for spec in data["channels"]:
            templates: dict[str, EnvelopeTemplate] = {}
            for name, template_spec in spec["envelope_templates"].items():
                samples = template_spec["samples"]
                templates[name] = EnvelopeTemplate(
                    name=name,
                    samples=tuple(samples),
                    sample_ms=template_spec["sample_ms"],
                )
            window = spec.get("packet_window_ms", [0, 0])
            channels.append(
                ChannelCapability(
                    channel=spec["channel"],
                    envelope_templates=templates,
                    min_event_interval_ms=spec["min_event_interval_ms"],
                    max_events_per_packet=spec["max_events_per_packet"],
                    packet_window_ms=(window[0], window[1]),
                )
            )
        return DeviceCapabilities(
            device_id=data["device_id"],
            capability_version=data["capability_version"],
            frame_ms=data["frame_ms"],
            channels=tuple(channels),
            schema_version=int(data.get("schema_version", 1)),
        )
    except (KeyError, TypeError, ValueError, IndexError) as exc:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                f"malformed capability manifest: {exc}",
                event_id="<capability>",
                field="<manifest>",
            )
        )
        return None


def validate_events(
    events: EventStream | Iterable[HapticEvent] | dict[str, Any],
    capabilities: DeviceCapabilities,
) -> ValidationResult:
    issues: list[Issue] = []
    if isinstance(events, EventStream):
        stream = events
    elif isinstance(events, dict):
        stream = _events_from_dict(events, issues)
        if stream is None:
            return ValidationResult(tuple(issues))
    else:
        stream = EventStream(events=tuple(events), capability_version=capabilities.capability_version)

    if stream.capability_version != capabilities.capability_version:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                "event stream capability_version does not match device manifest",
                event_id="<stream>",
                field="capability_version",
                details={
                    "event_stream": stream.capability_version,
                    "capability": capabilities.capability_version,
                },
            )
        )

    previous_frame: int | None = None
    current_frame: int | None = None
    previous_index = -1
    seen_ids: set[str] = set()
    max_channel = max((cap.channel for cap in capabilities.channels), default=-1)
    caps_by_channel = {cap.channel: cap for cap in capabilities.channels}
    for event in stream.events:
        if not isinstance(event.event_id, str) or not event.event_id:
            issues.append(
                Issue(
                    ErrorCode.E_SEQ,
                    "event_id must be a non-empty string",
                    event_id=event.event_id,
                    field="event_id",
                )
            )
        if event.event_id in seen_ids:
            issues.append(
                Issue(
                    ErrorCode.E_SEQ,
                    "duplicate event_id",
                    event_id=event.event_id,
                    field="event_id",
                )
            )
        seen_ids.add(event.event_id)

        if not isinstance(event.frame, int) or isinstance(event.frame, bool) or event.frame < 0:
            issues.append(
                Issue(
                    ErrorCode.E_SEQ,
                    "frame must be a non-negative integer",
                    event_id=event.event_id,
                    field="frame",
                )
            )
        else:
            if previous_frame is None:
                current_frame = event.frame
                previous_index = -1
            elif event.frame < previous_frame:
                issues.append(
                    Issue(
                        ErrorCode.E_SEQ,
                        "frames are out of order",
                        event_id=event.event_id,
                        field="frame",
                        details={"expected_at_least": previous_frame, "actual": event.frame},
                    )
                )
            elif event.frame > previous_frame + 1:
                issues.append(
                    Issue(
                        ErrorCode.E_SEQ,
                        "frame sequence has a gap",
                        event_id=event.event_id,
                        field="frame",
                        details={"expected": previous_frame + 1, "actual": event.frame},
                    )
                )
            elif event.frame > previous_frame:
                current_frame = event.frame
                previous_index = -1
            previous_frame = event.frame

        if not isinstance(event.frame_index, int) or isinstance(event.frame_index, bool) or event.frame_index < 0:
            issues.append(
                Issue(
                    ErrorCode.E_SEQ,
                    "frame_index must be a non-negative integer",
                    event_id=event.event_id,
                    field="frame_index",
                )
            )
        elif current_frame == event.frame:
            if event.frame_index <= previous_index:
                issues.append(
                    Issue(
                        ErrorCode.E_SEQ,
                        "frame_index must increase within each frame",
                        event_id=event.event_id,
                        field="frame_index",
                        details={"previous": previous_index, "actual": event.frame_index},
                    )
                )
            previous_index = event.frame_index

        if not isinstance(event.channel, int) or isinstance(event.channel, bool) or event.channel < 0:
            issues.append(
                Issue(
                    ErrorCode.E_CHAN,
                    "channel must be a non-negative integer",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="channel",
                )
            )
        elif event.channel > max_channel or event.channel not in caps_by_channel:
            issues.append(
                Issue(
                    ErrorCode.E_CHAN,
                    "channel exceeds device capability",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="channel",
                    details={"channel_count": len(capabilities.channels)},
                )
            )
        else:
            channel_cap = caps_by_channel[event.channel]
            if event.event_type not in channel_cap.envelope_templates:
                issues.append(
                    Issue(
                        ErrorCode.E_PARA,
                        "event type has no envelope template on the target channel",
                        event_id=event.event_id,
                        channel=event.channel,
                        field="type",
                        details={"event_type": event.event_type},
                    )
                )

        if not _number(event.intensity) or event.intensity < 0 or event.intensity > 1:
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "intensity must be finite and within [0, 1]",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="intensity",
                    details={"value": event.intensity},
                )
            )
        if (
            not isinstance(event.priority, int)
            or isinstance(event.priority, bool)
            or event.priority < 0
            or event.priority > 255
        ):
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "priority must be an integer within [0, 255]",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="priority",
                    details={"value": event.priority},
                )
            )
        if not isinstance(event.no_interrupt, bool):
            issues.append(
                Issue(
                    ErrorCode.E_PARA,
                    "no_interrupt must be a boolean",
                    event_id=event.event_id,
                    channel=event.channel,
                    field="no_interrupt",
                )
            )
    return ValidationResult(tuple(issues))


def _events_from_dict(data: dict[str, Any], issues: list[Issue]) -> EventStream | None:
    try:
        events = tuple(
            HapticEvent(
                event_id=item["event_id"],
                frame=item["frame"],
                frame_index=item["frame_index"],
                event_type=item["type"],
                channel=item["channel"],
                intensity=item["intensity"],
                priority=item["priority"],
                no_interrupt=item.get("no_interrupt", False),
            )
            for item in data["events"]
        )
        return EventStream(
            events=events,
            capability_version=data["capability_version"],
            schema_version=data.get("schema_version", 1),
        )
    except (KeyError, TypeError, ValueError) as exc:
        issues.append(
            Issue(
                ErrorCode.E_PARA,
                f"malformed event stream: {exc}",
                event_id="<stream>",
                field="<events>",
            )
        )
        return None


def load_capabilities(path: str | Path) -> tuple[DeviceCapabilities | None, ValidationResult]:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return None, ValidationResult(
            (
                Issue(
                    ErrorCode.E_PARA,
                    f"cannot load capability manifest: {exc}",
                    event_id="<capability>",
                    field="<file>",
                ),
            )
        )
    issues: list[Issue] = []
    capabilities = _capabilities_from_dict(raw, issues)
    if capabilities is None:
        return None, ValidationResult(tuple(issues))
    result = validate_capabilities(capabilities)
    return capabilities, result


def load_events(path: str | Path) -> tuple[EventStream | None, ValidationResult]:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return None, ValidationResult(
            (
                Issue(
                    ErrorCode.E_PARA,
                    f"cannot load event stream: {exc}",
                    event_id="<stream>",
                    field="<file>",
                ),
            )
        )
    issues: list[Issue] = []
    stream = _events_from_dict(raw, issues)
    if stream is None:
        return None, ValidationResult(tuple(issues))
    return stream, ValidationResult(tuple(issues))


def load_and_validate(
    events_path: str | Path,
    capabilities_path: str | Path,
) -> tuple[EventStream | None, DeviceCapabilities | None, ValidationResult]:
    capabilities, cap_result = load_capabilities(capabilities_path)
    if capabilities is None or not cap_result.ok:
        return None, capabilities, cap_result
    stream, event_result = load_events(events_path)
    if stream is None or not event_result.ok:
        return stream, capabilities, event_result
    result = validate_events(stream, capabilities)
    return stream, capabilities, result

