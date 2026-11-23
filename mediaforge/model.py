from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any, Iterable

from .errors import E_CHAP, E_CONFLICT, E_CORRUPT, E_TRACK, fail
from .jsonutil import canonical_json_bytes, sha256_bytes
from .timebase import Timebase


@dataclass(frozen=True)
class Frame:
    pts_ticks: int
    rows: int
    cols: int
    cells: tuple[str, ...]
    sha256: str

    def descriptor(self, include_hash: bool = True) -> dict[str, Any]:
        value: dict[str, Any] = {
            "pts_ticks": self.pts_ticks,
            "rows": self.rows,
            "cols": self.cols,
            "cells": list(self.cells),
        }
        if include_hash:
            value["sha256"] = self.sha256
        return value

    @classmethod
    def from_values(
        cls,
        *,
        pts_ticks: int,
        rows: int,
        cols: int,
        cells: Iterable[str],
    ) -> "Frame":
        cell_tuple = tuple(cells)
        body = {
            "pts_ticks": pts_ticks,
            "rows": rows,
            "cols": cols,
            "cells": list(cell_tuple),
        }
        return cls(
            pts_ticks=pts_ticks,
            rows=rows,
            cols=cols,
            cells=cell_tuple,
            sha256=sha256_bytes(canonical_json_bytes(body)),
        )


@dataclass(frozen=True)
class MediaTrack:
    track_id: str
    kind: str
    language: str
    timebase: Timebase
    sample_count: int
    duration_ticks: int
    codec: str
    payload: bytes = b""
    payload_encoding: str = ""
    width: int | None = None
    height: int | None = None
    frame_interval_ticks: int | None = None
    sample_rate: int | None = None
    channels: int | None = None
    sample_format: str | None = None
    languages: tuple[str, ...] = ()
    roles: tuple[str, ...] = ()
    hdr: str | None = None
    custom: dict[str, Any] = field(default_factory=dict)

    @property
    def payload_sha256(self) -> str:
        return sha256_bytes(self.payload)

    @property
    def frames(self) -> tuple[Frame, ...]:
        return decode_frame_payload(self.payload)

    def with_payload(self, payload: bytes, payload_encoding: str) -> "MediaTrack":
        return replace(self, payload=payload, payload_encoding=payload_encoding)

    def declaration(self) -> dict[str, Any]:
        value: dict[str, Any] = {
            "track_id": self.track_id,
            "kind": self.kind,
            "language": self.language,
            "timebase": self.timebase.to_dict(),
            "sample_count": self.sample_count,
            "duration_ticks": self.duration_ticks,
            "codec": self.codec,
        }
        optional = {
            "width": self.width,
            "height": self.height,
            "frame_interval_ticks": self.frame_interval_ticks,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "sample_format": self.sample_format,
            "hdr": self.hdr,
        }
        for key, item in optional.items():
            if item is not None:
                value[key] = item
        if self.roles:
            value["roles"] = list(self.roles)
        if self.languages and self.languages != (self.language,):
            value["languages"] = list(self.languages)
        if self.custom:
            value["custom"] = dict(sorted(self.custom.items()))
        return value


@dataclass(frozen=True)
class Chapter:
    chapter_id: str
    index: int
    start_ticks: int
    end_ticks: int
    title: str
    tags: dict[str, Any] = field(default_factory=dict)

    def declaration(self) -> dict[str, Any]:
        return {
            "chapter_id": self.chapter_id,
            "number": self.index,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "title": self.title,
            "tags": dict(sorted(self.tags.items())),
        }


@dataclass(frozen=True)
class ContainerModel:
    model_id: str
    title: str
    source_version: str
    timebase: Timebase
    duration_ticks: int
    tracks: tuple[MediaTrack, ...]
    chapters: tuple[Chapter, ...] = ()
    cover: bytes = b""
    cover_mime: str = "image/png"
    tags: dict[str, Any] = field(default_factory=dict)
    editorial: dict[str, Any] = field(default_factory=dict)

    def copy(self, **changes: Any) -> "ContainerModel":
        return replace(self, **changes)

    def track_by_id(self, track_id: str) -> MediaTrack | None:
        for track in self.tracks:
            if track.track_id == track_id:
                return track
        return None

    def replace_track(self, track: MediaTrack) -> "ContainerModel":
        tracks = tuple(track if item.track_id == track.track_id else item for item in self.tracks)
        return replace(self, tracks=tracks)

    def with_chapters(self, chapters: Iterable[Chapter]) -> "ContainerModel":
        return replace(self, chapters=tuple(chapters))


def decode_frame_payload(payload: bytes) -> tuple[Frame, ...]:
    if not payload:
        return ()
    import json

    try:
        values = json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return ()
    frames: list[Frame] = []
    for value in values:
        frames.append(
            Frame(
                pts_ticks=int(value["pts_ticks"]),
                rows=int(value["rows"]),
                cols=int(value["cols"]),
                cells=tuple(str(cell) for cell in value["cells"]),
                sha256=str(value["sha256"]),
            )
        )
    return tuple(frames)


def validate_chapters(model: ContainerModel) -> tuple[Chapter, ...]:
    if model.duration_ticks <= 0:
        fail(E_CHAP, "$.duration_ticks", "container duration must be positive")
    chapters = sorted(
        model.chapters,
        key=lambda chapter: (chapter.start_ticks, chapter.end_ticks, chapter.chapter_id),
    )
    previous: Chapter | None = None
    for position, chapter in enumerate(chapters):
        path = f"$.chapters[{position}]"
        number = chapter.index if chapter.index else position + 1
        if chapter.start_ticks < 0:
            fail(
                E_CHAP,
                f"{path}.start_ticks",
                f"chapter {number} starts before zero",
                measured=chapter.start_ticks,
                declared=0,
                details={"chapter_number": number, "chapter_id": chapter.chapter_id},
            )
        if chapter.end_ticks <= chapter.start_ticks:
            fail(
                E_CHAP,
                f"{path}.end_ticks",
                f"chapter {number} has a non-positive interval",
                measured=chapter.end_ticks,
                declared={"start_ticks": chapter.start_ticks},
                details={"chapter_number": number, "chapter_id": chapter.chapter_id},
            )
        if chapter.end_ticks > model.duration_ticks:
            fail(
                E_CHAP,
                f"{path}.end_ticks",
                f"chapter {number} exceeds the container duration",
                measured=chapter.end_ticks,
                declared=model.duration_ticks,
                details={"chapter_number": number, "chapter_id": chapter.chapter_id},
            )
        if previous is not None and chapter.start_ticks < previous.end_ticks:
            fail(
                E_CHAP,
                f"{path}.start_ticks",
                f"chapter {number} overlaps chapter {previous.index}",
                measured=chapter.start_ticks,
                declared={"previous_end_ticks": previous.end_ticks},
                details={
                    "chapter_number": number,
                    "chapter_id": chapter.chapter_id,
                    "previous_chapter_number": previous.index,
                    "previous_chapter_id": previous.chapter_id,
                },
            )
        previous = chapter
    return tuple(chapters)


def validate_tracks(model: ContainerModel, *, require_payload: bool = False) -> None:
    if not model.tracks:
        fail(E_TRACK, "$.tracks", "at least one track is required")
    seen: set[str] = set()
    for position, track in enumerate(model.tracks):
        path = f"$.tracks[{position}]"
        if track.track_id in seen:
            fail(E_CONFLICT, f"{path}.track_id", "duplicate track id", measured=track.track_id)
        seen.add(track.track_id)
        if track.kind not in {"video", "audio"}:
            fail(E_TRACK, f"{path}.kind", "unsupported track kind", measured=track.kind)
        if not track.language:
            fail(E_TRACK, f"{path}.language", "track language is required")
        if track.sample_count <= 0:
            fail(E_TRACK, f"{path}.sample_count", "track sample count must be positive")
        if track.duration_ticks <= 0:
            fail(E_TRACK, f"{path}.duration_ticks", "track duration must be positive")
        if track.timebase.num <= 0 or track.timebase.den <= 0:
            fail(E_TRACK, f"{path}.timebase", "track timebase must be positive")
        if require_payload and not track.payload:
            fail(E_TRACK, f"{path}.payload", "track payload is required")
        if track.kind == "video":
            if not track.payload_encoding and require_payload:
                fail(E_TRACK, f"{path}.payload_encoding", "video payload encoding is required")
        elif track.kind == "audio":
            if track.channels is None or track.sample_rate is None:
                fail(E_TRACK, f"{path}", "audio tracks require channels and sample_rate")


def validate_model(model: ContainerModel, *, require_payload: bool = False) -> None:
    validate_tracks(model, require_payload=require_payload)
    validate_chapters(model)
    if not model.model_id:
        fail(E_CONFLICT, "$.model_id", "model id is required")
    if model.cover and not model.cover.startswith(b"\x89PNG\r\n\x1a\n"):
        fail(E_CORRUPT, "$.cover", "cover is not a PNG image")
