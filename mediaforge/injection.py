from __future__ import annotations

import json
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from .errors import E_PARAM, fail
from .jsonutil import canonical_json_bytes
from .model import ContainerModel
from .raw import RawStream


@dataclass(frozen=True)
class InjectionConfig:
    conflict_track: str | None = None
    conflict_language: str = "ja-JP"
    order_track: str | None = None
    order_frame_index: int = 1
    missing_track: str | None = None
    corrupt_track: str | None = None
    corrupt_offset: int = 17
    parameter_version: str = "inject-p1"

    @property
    def active(self) -> tuple[str, ...]:
        modes = []
        if self.conflict_track:
            modes.append("conflict")
        if self.order_track:
            modes.append("order")
        if self.missing_track:
            modes.append("missing_track")
        if self.corrupt_track:
            modes.append("corrupt")
        return tuple(modes)

    def to_dict(self) -> dict[str, Any]:
        return {
            "conflict": {"track_id": self.conflict_track, "language": self.conflict_language}
            if self.conflict_track
            else None,
            "order": {"track_id": self.order_track, "frame_index": self.order_frame_index}
            if self.order_track
            else None,
            "missing_track": {"track_id": self.missing_track} if self.missing_track else None,
            "corrupt": {"track_id": self.corrupt_track, "offset": self.corrupt_offset}
            if self.corrupt_track
            else None,
            "parameter_version": self.parameter_version,
        }

    def without_input_corruption(self) -> "InjectionConfig":
        """Keep every input-stage fault while deferring corruption to the segment stage.

        The segmented packer injects ``corrupt_track`` itself when serializing each
        segment, so the raw-stream mutation must be skipped during sample import.
        Dropping the whole configuration instead would silently discard the other
        requested injections (metadata conflict, timestamp inversion, missing track).
        """
        return replace(
            self,
            corrupt_track=None,
            corrupt_offset=17,
        )

    @classmethod
    def from_dict(cls, value: Any) -> "InjectionConfig":
        if value is None:
            return cls()
        if not isinstance(value, dict):
            fail(E_PARAM, "$.injection", "injection configuration must be an object")
        root = value.get("injection", value)
        if not isinstance(root, dict):
            fail(E_PARAM, "$.injection", "injection configuration must be an object")
        mode = root.get("mode")
        if mode:
            root = {**root, str(mode): root}
        conflict = root.get("metadata_conflict", root.get("conflict"))
        order = root.get("timestamp_inversion", root.get("order"))
        missing = root.get("missing_track", root.get("missing"))
        corrupt = root.get("corrupt_segment", root.get("corrupt"))
        return cls(
            conflict_track=_track_id(conflict, "$.injection.metadata_conflict"),
            conflict_language=_language(conflict),
            order_track=_track_id(order, "$.injection.timestamp_inversion"),
            order_frame_index=_frame_index(order),
            missing_track=_track_id(missing, "$.injection.missing_track"),
            corrupt_track=_track_id(corrupt, "$.injection.corrupt_segment"),
            corrupt_offset=_offset(corrupt),
            parameter_version=str(root.get("parameter_version", "inject-p1")),
        )

    @classmethod
    def from_file(cls, path: str | Path) -> "InjectionConfig":
        try:
            with open(path, "r", encoding="utf-8") as handle:
                value = json.load(handle)
        except OSError as exc:
            fail(E_PARAM, "$.injection", f"cannot read injection file: {exc}")
        except ValueError as exc:
            fail(E_PARAM, "$.injection", f"injection file is not valid JSON: {exc}")
        return cls.from_dict(value)


def _track_id(value: Any, path: str) -> str | None:
    if value is None or value is False:
        return None
    if value is True:
        fail(E_PARAM, path, "a track id is required")
    if isinstance(value, str):
        return value
    if not isinstance(value, dict):
        fail(E_PARAM, path, "injection entry must be an object or track id")
    enabled = value.get("enabled", True)
    if not enabled:
        return None
    track_id = value.get("track_id", value.get("track"))
    if not isinstance(track_id, str) or not track_id:
        fail(E_PARAM, f"{path}.track_id", "a track id is required")
    return track_id


def _language(value: Any) -> str:
    if isinstance(value, dict):
        language = value.get("language", value.get("value", "ja-JP"))
        return str(language)
    return "ja-JP"


def _frame_index(value: Any) -> int:
    if isinstance(value, dict):
        index = value.get("frame_index", value.get("index", 1))
        if isinstance(index, bool) or not isinstance(index, int) or index < 1:
            fail(E_PARAM, "$.injection.timestamp_inversion.frame_index", "frame index must be >= 1")
        return index
    return 1


def _offset(value: Any) -> int:
    if isinstance(value, dict):
        offset = value.get("offset", value.get("byte_offset", 17))
        if isinstance(offset, bool) or not isinstance(offset, int) or offset < 0:
            fail(E_PARAM, "$.injection.corrupt_segment.offset", "corruption offset must be >= 0")
        return offset
    return 17


def apply_input_injections(
    model: ContainerModel,
    streams: dict[str, RawStream],
    config: InjectionConfig | None,
) -> tuple[ContainerModel, dict[str, RawStream]]:
    if config is None:
        return model, streams
    result_model = model
    result_streams = dict(streams)
    if config.conflict_track:
        found = False
        updated = []
        for track in result_model.tracks:
            if track.track_id == config.conflict_track:
                updated.append(replace(track, language=config.conflict_language))
                found = True
            else:
                updated.append(track)
        if not found:
            fail(
                E_PARAM,
                "$.injection.metadata_conflict.track_id",
                "injected conflict track does not exist",
                measured=config.conflict_track,
            )
        result_model = result_model.copy(tracks=tuple(updated))
    if config.order_track:
        stream = result_streams.get(config.order_track)
        if stream is None:
            fail(
                E_PARAM,
                "$.injection.timestamp_inversion.track_id",
                "injected order track does not exist",
                measured=config.order_track,
            )
        result_streams[config.order_track] = _invert_timestamps(stream, config.order_frame_index)
    if config.missing_track:
        if config.missing_track not in result_streams:
            fail(
                E_PARAM,
                "$.injection.missing_track.track_id",
                "injected missing track does not exist",
                measured=config.missing_track,
            )
        result_streams.pop(config.missing_track)
    if config.corrupt_track:
        stream = result_streams.get(config.corrupt_track)
        if stream is None:
            fail(
                E_PARAM,
                "$.injection.corrupt_segment.track_id",
                "injected corruption track does not exist",
                measured=config.corrupt_track,
            )
        result_streams[config.corrupt_track] = _corrupt_stream(stream, config.corrupt_offset)
    return result_model, result_streams


def _invert_timestamps(stream: RawStream, frame_index: int) -> RawStream:
    if stream.kind != "video":
        fail(
            E_PARAM,
            "$.injection.timestamp_inversion.track_id",
            "timestamp inversion requires a video track",
            measured=stream.kind,
        )
    frames = json.loads(stream.payload.decode("utf-8"))
    if frame_index >= len(frames):
        fail(
            E_PARAM,
            "$.injection.timestamp_inversion.frame_index",
            "frame index is outside the video stream",
            measured=frame_index,
            declared=len(frames),
        )
    previous_pts = int(frames[frame_index - 1]["pts_ticks"])
    frames[frame_index]["pts_ticks"] = previous_pts
    body = {
        key: frames[frame_index][key]
        for key in ("pts_ticks", "rows", "cols", "cells")
    }
    from .jsonutil import sha256_bytes

    frames[frame_index]["sha256"] = sha256_bytes(canonical_json_bytes(body))
    return replace(stream, payload=canonical_json_bytes(frames))


def _corrupt_stream(stream: RawStream, offset: int) -> RawStream:
    if not stream.payload:
        fail(
            E_PARAM,
            "$.injection.corrupt_segment",
            "cannot corrupt an empty payload",
            measured=stream.track_id,
        )
    position = offset % len(stream.payload)
    payload = bytearray(stream.payload)
    payload[position] ^= 0x5A
    return replace(stream, payload=bytes(payload))
