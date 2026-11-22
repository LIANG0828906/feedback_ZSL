from __future__ import annotations

import json
from pathlib import Path

from .models import (
    ChannelCapability,
    DeviceCapabilities,
    EnvelopeTemplate,
    EventStream,
    HapticEvent,
    canonical_json,
)


def sample_capabilities() -> DeviceCapabilities:
    templates = {
        "hit": EnvelopeTemplate("hit", (0.15, 0.70, 1.0, 0.55, 0.15), 8.0),
        "shoot": EnvelopeTemplate("shoot", (1.0, 0.65, 0.20), 8.0),
        "explosion": EnvelopeTemplate("explosion", (0.20, 0.90, 1.0, 0.65, 0.25, 0.05), 8.0),
        "footstep": EnvelopeTemplate("footstep", (0.10, 0.35, 0.20), 8.0),
        "ui": EnvelopeTemplate("ui", (0.60, 0.60), 8.0),
    }
    channel_zero = ChannelCapability(
        channel=0,
        envelope_templates=dict(templates),
        min_event_interval_ms=32.0,
        max_events_per_packet=3,
        packet_window_ms=(0.0, 64.0),
    )
    channel_one = ChannelCapability(
        channel=1,
        envelope_templates={
            "hit": EnvelopeTemplate("hit", (0.20, 0.80, 0.50), 8.0),
            "shoot": EnvelopeTemplate("shoot", (0.90, 0.40), 8.0),
            "explosion": EnvelopeTemplate("explosion", (0.30, 1.0, 0.70, 0.20), 8.0),
            "footstep": EnvelopeTemplate("footstep", (0.15, 0.30), 8.0),
            "ui": EnvelopeTemplate("ui", (0.50, 0.50), 8.0),
        },
        min_event_interval_ms=24.0,
        max_events_per_packet=2,
        packet_window_ms=(0.0, 48.0),
    )
    return DeviceCapabilities(
        device_id="loopback-haptic-01",
        capability_version=7,
        frame_ms=16.0,
        channels=(channel_zero, channel_one),
    )


def sample_events() -> EventStream:
    rows = [
        (0, "hit", 0, 0.70, 2, False),
        (0, "footstep", 1, 0.30, 0, False),
        (1, "footstep", 0, 0.20, 0, False),
        (2, "hit", 0, 0.90, 6, False),
        (3, "shoot", 0, 0.80, 3, False),
        (4, "hit", 0, 0.40, 1, False),
        (5, "explosion", 0, 0.95, 8, True),
        (5, "shoot", 1, 0.70, 3, False),
        (6, "hit", 0, 0.60, 4, False),
        (7, "explosion", 0, 0.80, 7, False),
        (8, "hit", 0, 0.90, 10, False),
        (9, "explosion", 1, 0.90, 9, True),
        (10, "hit", 1, 0.70, 2, False),
        (11, "hit", 0, 0.40, 1, False),
        (12, "ui", 0, 0.50, 2, False),
        (13, "shoot", 1, 0.80, 7, False),
        (14, "explosion", 0, 0.90, 9, True),
        (15, "hit", 0, 0.80, 5, False),
        (16, "ui", 0, 0.40, 1, False),
        (17, "hit", 1, 0.60, 1, False),
        (18, "shoot", 0, 0.70, 6, False),
        (19, "footstep", 0, 0.20, 0, False),
        (20, "explosion", 1, 0.70, 5, True),
        (21, "hit", 0, 0.50, 2, False),
        (22, "ui", 1, 0.40, 3, True),
        (23, "shoot", 0, 0.60, 9, False),
    ]
    events: list[HapticEvent] = []
    frame_indexes: dict[int, int] = {}
    for frame, event_type, channel, intensity, priority, no_interrupt in rows:
        index = frame_indexes.get(frame, 0)
        frame_indexes[frame] = index + 1
        events.append(
            HapticEvent(
                event_id=f"E{frame:04d}_{index:02d}",
                frame=frame,
                frame_index=index,
                event_type=event_type,
                channel=channel,
                intensity=intensity,
                priority=priority,
                no_interrupt=no_interrupt,
            )
        )
    return EventStream(
        events=tuple(events),
        capability_version=7,
        source="deterministic-sample",
    )


def write_sample_files(directory: str | Path) -> tuple[Path, Path]:
    target = Path(directory)
    target.mkdir(parents=True, exist_ok=True)
    capabilities_path = target / "capabilities.json"
    events_path = target / "events.json"
    capabilities_path.write_text(
        json.dumps(sample_capabilities().to_dict(), ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    events_path.write_text(
        json.dumps(sample_events().to_dict(), ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    (target / "fault_plan.json").write_text(
        json.dumps(
            {
                "drops": [3],
                "duplicates": [6],
                "delays_ms": [[8, 120]],
                "reorder_pairs": [[9, 10]],
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )
    return events_path, capabilities_path


def sample_digest() -> str:
    from .models import sha256_hex

    payload = {
        "events": sample_events().to_dict(),
        "capabilities": sample_capabilities().to_dict(),
    }
    return sha256_hex(canonical_json(payload))

