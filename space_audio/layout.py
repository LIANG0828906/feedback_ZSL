from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .errors import ErrorCode, WorkbenchError, meta_error, para_error


@dataclass(frozen=True)
class ChannelSpec:
    id: str
    azimuth_deg: float
    elevation_deg: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "azimuth_deg": self.azimuth_deg,
            "elevation_deg": self.elevation_deg,
        }


@dataclass(frozen=True)
class ChannelLayout:
    name: str
    channels: tuple[ChannelSpec, ...]

    @property
    def count(self) -> int:
        return len(self.channels)

    @property
    def ids(self) -> tuple[str, ...]:
        return tuple(channel.id for channel in self.channels)

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "channels": [channel.to_dict() for channel in self.channels],
        }


KNOWN_LAYOUTS: dict[str, tuple[ChannelSpec, ...]] = {
    "mono": (ChannelSpec("M", 0.0),),
    "stereo": (ChannelSpec("L", -30.0), ChannelSpec("R", 30.0)),
    "quad": (
        ChannelSpec("L", -45.0),
        ChannelSpec("R", 45.0),
        ChannelSpec("Ls", -135.0),
        ChannelSpec("Rs", 135.0),
    ),
    "5.1": (
        ChannelSpec("L", -30.0),
        ChannelSpec("R", 30.0),
        ChannelSpec("C", 0.0),
        ChannelSpec("LFE", 0.0),
        ChannelSpec("Ls", -110.0),
        ChannelSpec("Rs", 110.0),
    ),
    "7.1": (
        ChannelSpec("L", -30.0),
        ChannelSpec("R", 30.0),
        ChannelSpec("C", 0.0),
        ChannelSpec("LFE", 0.0),
        ChannelSpec("Ls", -90.0),
        ChannelSpec("Rs", 90.0),
        ChannelSpec("Lb", -150.0),
        ChannelSpec("Rb", 150.0),
    ),
}


def get_layout(name: str) -> ChannelLayout:
    if not isinstance(name, str) or not name:
        raise para_error("layout name must be a non-empty string", "channel_layout")
    if name in KNOWN_LAYOUTS:
        return ChannelLayout(name, KNOWN_LAYOUTS[name])
    if name.startswith("custom:"):
        ids = tuple(item for item in name[7:].split(",") if item)
        if not ids:
            raise para_error("custom layout has no channels", "channel_layout")
        return ChannelLayout(
            name,
            tuple(ChannelSpec(item, 0.0) for item in ids),
        )
    raise para_error(f"unknown channel layout {name!r}", "channel_layout")


def parse_layout(payload: Any, field: str = "channel_layout") -> ChannelLayout:
    if payload is None:
        raise meta_error("channel layout metadata is missing", field)

    if isinstance(payload, str):
        return get_layout(payload)

    if not isinstance(payload, dict):
        raise meta_error("channel layout metadata must be an object or string", field)

    name = payload.get("name")
    channels_payload = payload.get("channels")
    if not isinstance(name, str) or not name:
        raise meta_error("channel layout name is missing", f"{field}.name")
    if not isinstance(channels_payload, (list, tuple)) or not channels_payload:
        raise meta_error("channel layout channel list is missing", f"{field}.channels")

    channels: list[ChannelSpec] = []
    for index, item in enumerate(channels_payload):
        if isinstance(item, str):
            channels.append(ChannelSpec(item, 0.0))
            continue
        if not isinstance(item, dict):
            raise meta_error(
                "channel descriptor must be a string or object",
                f"{field}.channels[{index}]",
            )
        identifier = item.get("id")
        if not isinstance(identifier, str) or not identifier:
            raise meta_error(
                "channel id is missing",
                f"{field}.channels[{index}].id",
            )
        azimuth = item.get("azimuth_deg", 0.0)
        elevation = item.get("elevation_deg", 0.0)
        try:
            azimuth_f = float(azimuth)
            elevation_f = float(elevation)
        except (TypeError, ValueError) as exc:
            raise meta_error(
                "channel azimuth/elevation must be numeric",
                f"{field}.channels[{index}]",
            ) from exc
        channels.append(ChannelSpec(identifier, azimuth_f, elevation_f))

    ids = [channel.id for channel in channels]
    if len(set(ids)) != len(ids):
        raise WorkbenchError(
            ErrorCode.CHAN,
            "duplicate channel identifiers in declared layout",
            field=f"{field}.channels",
            details={"channel_ids": ids},
        )
    return ChannelLayout(name, tuple(channels))


def validate_layout_mapping(
    layout: ChannelLayout,
    channel_map: Iterable[int] | None,
    dry_channel_count: int,
) -> tuple[int, ...]:
    if channel_map is None:
        if dry_channel_count <= 0:
            raise para_error("dry clip has no channels", "dry")
        return tuple(index % dry_channel_count for index in range(layout.count))

    try:
        mapping = tuple(int(item) for item in channel_map)
    except (TypeError, ValueError) as exc:
        raise para_error("channel_map must contain integer indices", "channel_map") from exc

    if len(mapping) != layout.count:
        raise WorkbenchError(
            ErrorCode.CHAN,
            "channel_map length does not match declared output layout",
            field="channel_map",
            details={
                "mapping_channels": len(mapping),
                "layout_channels": layout.count,
                "layout": layout.name,
            },
        )
    for index in mapping:
        if index < 0 or index >= dry_channel_count:
            raise para_error(
                "channel_map source index is outside the dry clip",
                "channel_map",
                details={"index": index, "dry_channels": dry_channel_count},
            )
    return mapping
