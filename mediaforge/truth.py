from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any

from .errors import E_CORRUPT, E_IO, E_META, fail
from .jsonutil import canonical_json_bytes, read_json, sha256_bytes
from .timebase import Timebase


@dataclass(frozen=True)
class TruthTrack:
    track_id: str
    kind: str
    sample_count: int
    timebase: Timebase
    duration_ticks: int
    payload_sha256: str
    language: str
    width: int | None = None
    height: int | None = None
    frame_interval_ticks: int | None = None
    sample_rate: int | None = None
    channels: int | None = None


@dataclass(frozen=True)
class TruthChapter:
    chapter_id: str
    number: int
    start_ticks: int
    end_ticks: int
    title: str
    tags: dict[str, Any]


@dataclass(frozen=True)
class TruthManifest:
    sample_id: str
    seed: int
    timebase: Timebase
    duration_ticks: int
    tracks: tuple[TruthTrack, ...]
    chapters: tuple[TruthChapter, ...]
    tags: dict[str, Any]
    cover_sha256: str
    fingerprint: str
    metadata_versions: tuple[str, ...]
    raw: dict[str, Any]

    @property
    def duration_seconds(self) -> Fraction:
        return self.timebase.seconds(self.duration_ticks)


def compute_truth_fingerprint(value: dict[str, Any]) -> str:
    body = dict(value)
    body.pop("fingerprint", None)
    return sha256_bytes(canonical_json_bytes(body))


def parse_truth(value: dict[str, Any], *, verify_fingerprint: bool = True) -> TruthManifest:
    if not isinstance(value, dict):
        fail(E_META, "$", "truth manifest must be an object")
    fingerprint = value.get("fingerprint")
    if not isinstance(fingerprint, str):
        fail(E_META, "$.fingerprint", "truth fingerprint is required")
    if verify_fingerprint:
        measured = compute_truth_fingerprint(value)
        if measured != fingerprint:
            fail(
                E_CORRUPT,
                "$.fingerprint",
                "truth manifest fingerprint does not match its contents",
                measured=measured,
                declared=fingerprint,
            )
    try:
        timebase = Timebase.from_dict(value["timebase"], "$.timebase")
        tracks = tuple(
            TruthTrack(
                track_id=str(item["track_id"]),
                kind=str(item["kind"]),
                sample_count=int(item["sample_count"]),
                timebase=Timebase.from_dict(item["timebase"], f"$.tracks[{index}].timebase"),
                duration_ticks=int(item["duration_ticks"]),
                payload_sha256=str(item["payload_sha256"]),
                language=str(item.get("language", "")),
                width=item.get("width"),
                height=item.get("height"),
                frame_interval_ticks=item.get("frame_interval_ticks"),
                sample_rate=item.get("sample_rate"),
                channels=item.get("channels"),
            )
            for index, item in enumerate(value["tracks"])
        )
        chapters = tuple(
            TruthChapter(
                chapter_id=str(item["chapter_id"]),
                number=int(item["number"]),
                start_ticks=int(item["start_ticks"]),
                end_ticks=int(item["end_ticks"]),
                title=str(item.get("title", "")),
                tags=dict(item.get("tags", {})),
            )
            for item in value["chapters"]
        )
        return TruthManifest(
            sample_id=str(value["sample_id"]),
            seed=int(value["seed"]),
            timebase=timebase,
            duration_ticks=int(value["duration_ticks"]),
            tracks=tracks,
            chapters=chapters,
            tags=dict(value.get("tags", {})),
            cover_sha256=str(value.get("cover_sha256", "")),
            fingerprint=fingerprint,
            metadata_versions=tuple(str(item) for item in value.get("metadata_versions", [])),
            raw=value,
        )
    except KeyError as exc:
        fail(E_META, f"$.{exc.args[0]}", "required truth field is missing")
    except (TypeError, ValueError) as exc:
        fail(E_META, "$", f"invalid truth manifest: {exc}")


def load_truth(path: str | Path, *, verify_fingerprint: bool = True) -> TruthManifest:
    try:
        value = read_json(path)
    except OSError as exc:
        fail(E_IO, "$", f"cannot read truth manifest: {exc}")
    return parse_truth(value, verify_fingerprint=verify_fingerprint)
