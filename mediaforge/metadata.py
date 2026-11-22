from __future__ import annotations

from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from .errors import (
    E_LOSSY,
    E_META,
    E_PARAM,
    E_VER,
    LossyWarning,
    WorkbenchError,
    fail,
)
from .jsonutil import decode_base64, encode_base64, read_json
from .model import Chapter, ContainerModel, MediaTrack, validate_chapters
from .timebase import Timebase


SUPPORTED_VERSIONS = ("v1", "v2", "v3")
CHAPTER_TIMEBASE = Timebase(1, 600)


@dataclass(frozen=True)
class MetadataExport:
    target_version: str
    metadata: dict[str, Any]
    warnings: tuple[LossyWarning, ...]
    trace: tuple[dict[str, Any], ...]

    @property
    def ok(self) -> bool:
        return True

    def warning_dicts(self) -> list[dict[str, Any]]:
        return [warning.to_dict() for warning in self.warnings]


def normalize_source_version(value: Any, path: str = "$.metadata_version") -> str:
    if isinstance(value, str):
        lowered = value.lower()
        if lowered in SUPPORTED_VERSIONS:
            return lowered
        if lowered.isdigit():
            value = int(lowered)
        elif lowered.startswith("v") and lowered[1:].isdigit():
            value = int(lowered[1:])
    if isinstance(value, bool) or not isinstance(value, int):
        fail(E_META, path, "metadata version must be 1, 2, 3, v1, v2, or v3", measured=value)
    if value not in (1, 2, 3):
        fail(E_META, path, "unknown metadata version", measured=value, declared=list(SUPPORTED_VERSIONS))
    return f"v{value}"


def normalize_target_version(value: Any, path: str = "$.target_version") -> str:
    if isinstance(value, str):
        lowered = value.lower()
        if lowered in SUPPORTED_VERSIONS:
            return lowered
        if lowered.isdigit():
            value = int(lowered)
        elif lowered.startswith("v") and lowered[1:].isdigit():
            value = int(lowered[1:])
    if isinstance(value, bool) or not isinstance(value, int):
        fail(E_VER, path, "target version must be 1, 2, 3, v1, v2, or v3", measured=value)
    if value not in (1, 2, 3):
        fail(E_VER, path, "unknown target version", measured=value, declared=list(SUPPORTED_VERSIONS))
    return f"v{value}"


def _mapping(value: Any, path: str, *, required: bool = True) -> dict[str, Any]:
    if value is None and not required:
        return {}
    if not isinstance(value, dict):
        fail(E_META, path, "expected an object")
    return value


def _sequence(value: Any, path: str) -> list[Any]:
    if not isinstance(value, list):
        fail(E_META, path, "expected an array")
    return value


def _required(mapping: dict[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        fail(E_META, f"{path}.{key}", "required field is missing")
    return mapping[key]


def _string(mapping: dict[str, Any], key: str, path: str) -> str:
    value = _required(mapping, key, path)
    if not isinstance(value, str) or not value:
        fail(E_META, f"{path}.{key}", "expected a non-empty string", measured=value)
    return value


def _integer(mapping: dict[str, Any], key: str, path: str) -> int:
    value = _required(mapping, key, path)
    if isinstance(value, bool) or not isinstance(value, int):
        fail(E_META, f"{path}.{key}", "expected an integer", measured=value)
    return value


def _optional_integer(mapping: dict[str, Any], key: str, path: str) -> int | None:
    value = mapping.get(key)
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int):
        fail(E_META, f"{path}.{key}", "expected an integer", measured=value)
    return value


def _timebase(mapping: dict[str, Any], key: str, path: str) -> Timebase:
    value = _required(mapping, key, path)
    try:
        return Timebase.from_dict(value, f"{path}.{key}")
    except WorkbenchError as exc:
        fail(E_META, f"{path}.{key}", f"invalid timebase: {exc.message}", measured=value)
    except (TypeError, ValueError) as exc:
        fail(E_META, f"{path}.{key}", f"invalid timebase: {exc}")


def _tags(value: Any, path: str) -> dict[str, Any]:
    if value is None:
        return {}
    if not isinstance(value, dict):
        fail(E_META, path, "custom tags must be an object")
    return dict(value)


def _cover_from_object(value: Any, path: str) -> tuple[bytes, str]:
    cover = _mapping(value, path, required=False)
    if not cover:
        return b"", "image/png"
    mime = str(cover.get("mime", cover.get("mime_type", "image/png")))
    encoded = cover.get("data", cover.get("base64", cover.get("data_base64")))
    if not isinstance(encoded, str):
        fail(E_META, f"{path}.data", "cover data must be a base64 string")
    try:
        return decode_base64(encoded), mime
    except (ValueError, TypeError) as exc:
        fail(E_META, f"{path}.data", f"invalid cover base64: {exc}")


def _track_common(
    *,
    path: str,
    track_id: str,
    kind: str,
    language: str,
    timebase: Timebase,
    sample_count: int,
    duration_ticks: int,
    codec: str,
    width: int | None = None,
    height: int | None = None,
    frame_interval_ticks: int | None = None,
    sample_rate: int | None = None,
    channels: int | None = None,
    sample_format: str | None = None,
    languages: tuple[str, ...] | None = None,
    roles: tuple[str, ...] = (),
    hdr: str | None = None,
    custom: dict[str, Any] | None = None,
) -> MediaTrack:
    if kind not in {"video", "audio"}:
        fail(E_META, f"{path}.kind", "track kind must be video or audio", measured=kind)
    if kind == "video":
        if width is None:
            fail(E_META, f"{path}.video.width", "video width is required")
        if height is None:
            fail(E_META, f"{path}.video.height", "video height is required")
        if frame_interval_ticks is None:
            fail(E_META, f"{path}.video.frame_interval_ticks", "video frame interval is required")
        payload_encoding = "json.frames.v1"
    else:
        if sample_rate is None:
            fail(E_META, f"{path}.audio.sample_rate", "audio sample rate is required")
        if channels is None:
            fail(E_META, f"{path}.audio.channels", "audio channels are required")
        payload_encoding = "pcm.s16le"
    return MediaTrack(
        track_id=track_id,
        kind=kind,
        language=language,
        timebase=timebase,
        sample_count=sample_count,
        duration_ticks=duration_ticks,
        codec=codec,
        payload=b"",
        payload_encoding=payload_encoding,
        width=width,
        height=height,
        frame_interval_ticks=frame_interval_ticks,
        sample_rate=sample_rate,
        channels=channels,
        sample_format=sample_format,
        languages=tuple(languages or (language,)),
        roles=roles,
        hdr=hdr,
        custom=dict(custom or {}),
    )


def load_metadata(path: str | Path) -> ContainerModel:
    try:
        value = read_json(path)
    except OSError as exc:
        fail(E_META, "$", f"cannot read metadata file: {exc}")
    except ValueError as exc:
        fail(E_META, "$", f"metadata file is not valid JSON: {exc}")
    return load_metadata_dict(value)


def load_metadata_dict(value: Any) -> ContainerModel:
    root = _mapping(value, "$")
    version = normalize_source_version(_required(root, "metadata_version", "$"))
    if version == "v1":
        return _load_v1(root)
    if version == "v2":
        return _load_v2(root)
    return _load_v3(root)


def _load_v1(root: dict[str, Any]) -> ContainerModel:
    timeline_tb = _timebase(root, "timeline_timebase", "$")
    duration_ms = _integer(root, "duration_ms", "$")
    duration_ticks = timeline_tb.ticks_from_units(duration_ms, 1000, "$.duration_ms")
    tracks: list[MediaTrack] = []
    raw_tracks = _sequence(_required(root, "tracks", "$"), "$.tracks")
    for index, value in enumerate(raw_tracks):
        path = f"$.tracks[{index}]"
        item = _mapping(value, path)
        track_tb = _timebase(item, "timebase", path)
        kind = _string(item, "type", path)
        video = _mapping(item.get("video"), f"{path}.video", required=False)
        audio = _mapping(item.get("audio"), f"{path}.audio", required=False)
        tracks.append(
            _track_common(
                path=path,
                track_id=_string(item, "track_id", path),
                kind=kind,
                language=_string(item, "language", path),
                timebase=track_tb,
                sample_count=_integer(item, "sample_count", path),
                duration_ticks=track_tb.ticks_from_units(_integer(item, "duration_ms", path), 1000, f"{path}.duration_ms"),
                codec=_string(item, "codec", path),
                width=_optional_integer(video, "width", f"{path}.video") if video else None,
                height=_optional_integer(video, "height", f"{path}.video") if video else None,
                frame_interval_ticks=_optional_integer(video, "frame_interval_ticks", f"{path}.video") if video else None,
                sample_rate=_optional_integer(audio, "sample_rate", f"{path}.audio") if audio else None,
                channels=_optional_integer(audio, "channels", f"{path}.audio") if audio else None,
                sample_format=str(audio.get("sample_format", "s16le")) if audio else None,
            )
        )
    chapters: list[Chapter] = []
    raw_chapters = _sequence(root.get("chapters", []), "$.chapters")
    for position, value in enumerate(raw_chapters):
        path = f"$.chapters[{position}]"
        item = _mapping(value, path)
        number = _integer(item, "number", path)
        chapters.append(
            Chapter(
                chapter_id=str(item.get("chapter_id", f"ch{number}")),
                index=number,
                start_ticks=timeline_tb.ticks_from_units(_integer(item, "start_ms", path), 1000, f"{path}.start_ms"),
                end_ticks=timeline_tb.ticks_from_units(_integer(item, "end_ms", path), 1000, f"{path}.end_ms"),
                title=str(item.get("label", item.get("title", ""))),
                tags={},
            )
        )
    cover, cover_mime = _cover_from_object(root.get("cover"), "$.cover")
    return ContainerModel(
        model_id=_string(root, "asset_id", "$"),
        title=_string(root, "title", "$"),
        source_version="v1",
        timebase=timeline_tb,
        duration_ticks=duration_ticks,
        tracks=tuple(tracks),
        chapters=tuple(sorted(chapters, key=lambda item: (item.start_ticks, item.end_ticks, item.index))),
        cover=cover,
        cover_mime=cover_mime,
        tags=_tags(root.get("tags"), "$.tags"),
        editorial={},
    )


def _load_v2(root: dict[str, Any]) -> ContainerModel:
    asset = _mapping(_required(root, "asset", "$"), "$.asset")
    timeline = _mapping(_required(root, "timeline", "$"), "$.timeline")
    timeline_tb = _timebase(timeline, "timebase", "$.timeline")
    duration_ticks = _integer(timeline, "duration_ticks", "$.timeline")
    tracks: list[MediaTrack] = []
    raw_tracks = _sequence(_required(root, "tracks", "$"), "$.tracks")
    for index, value in enumerate(raw_tracks):
        path = f"$.tracks[{index}]"
        item = _mapping(value, path)
        track_tb = _timebase(item, "timebase", path)
        video = _mapping(item.get("video"), f"{path}.video", required=False)
        audio = _mapping(item.get("audio"), f"{path}.audio", required=False)
        roles_value = item.get("roles", [])
        roles = tuple(str(role) for role in _sequence(roles_value, f"{path}.roles"))
        tracks.append(
            _track_common(
                path=path,
                track_id=_string(item, "track_id", path),
                kind=_string(item, "kind", path),
                language=_string(item, "language_tag", path),
                timebase=track_tb,
                sample_count=_integer(item, "sample_count", path),
                duration_ticks=_integer(item, "duration_ticks", path),
                codec=_string(item, "codec", path),
                width=_optional_integer(video, "width", f"{path}.video") if video else None,
                height=_optional_integer(video, "height", f"{path}.video") if video else None,
                frame_interval_ticks=_optional_integer(video, "frame_interval_ticks", f"{path}.video") if video else None,
                sample_rate=_optional_integer(audio, "sample_rate", f"{path}.audio") if audio else None,
                channels=_optional_integer(audio, "channels", f"{path}.audio") if audio else None,
                sample_format=str(audio.get("sample_format", "s16le")) if audio else None,
                roles=roles,
            )
        )
    chapters: list[Chapter] = []
    chapter_table = _mapping(root.get("chapters", {}), "$.chapters")
    for key in sorted(chapter_table):
        item = _mapping(chapter_table[key], f"$.chapters.{key}")
        chapters.append(
            Chapter(
                chapter_id=str(item.get("chapter_id", key)),
                index=_integer(item, "index", f"$.chapters.{key}"),
                start_ticks=_integer(item, "start_ticks", f"$.chapters.{key}"),
                end_ticks=_integer(item, "end_ticks", f"$.chapters.{key}"),
                title=str(item.get("title", item.get("name", ""))),
                tags={},
            )
        )
    assets = _mapping(root.get("assets"), "$.assets", required=False)
    cover, cover_mime = _cover_from_object(assets.get("cover"), "$.assets.cover")
    metadata = _mapping(root.get("metadata"), "$.metadata", required=False)
    return ContainerModel(
        model_id=_string(asset, "asset_id", "$.asset"),
        title=_string(asset, "title", "$.asset"),
        source_version="v2",
        timebase=timeline_tb,
        duration_ticks=duration_ticks,
        tracks=tuple(tracks),
        chapters=tuple(sorted(chapters, key=lambda item: (item.start_ticks, item.end_ticks, item.index))),
        cover=cover,
        cover_mime=cover_mime,
        tags=_tags(metadata.get("tags"), "$.metadata.tags"),
        editorial={},
    )


def _load_v3(root: dict[str, Any]) -> ContainerModel:
    timeline = _mapping(_required(root, "timeline", "$"), "$.timeline")
    timeline_tb = _timebase(timeline, "timebase", "$.timeline")
    duration_us = _integer(timeline, "duration_us", "$.timeline")
    duration_ticks = timeline_tb.ticks_from_units(duration_us, 1_000_000, "$.timeline.duration_us")
    tracks: list[MediaTrack] = []
    raw_tracks = _sequence(_required(root, "tracks", "$"), "$.tracks")
    for index, value in enumerate(raw_tracks):
        path = f"$.tracks[{index}]"
        item = _mapping(value, path)
        track_tb = _timebase(item, "time_base", path)
        languages = _sequence(_required(item, "languages", path), f"{path}.languages")
        if not languages or any(not isinstance(language, str) or not language for language in languages):
            fail(E_META, f"{path}.languages", "at least one language tag is required", measured=languages)
        primary = str(item.get("primary_language", languages[0] if languages else ""))
        if not primary:
            fail(E_META, f"{path}.primary_language", "primary language is required")
        video = _mapping(item.get("video"), f"{path}.video", required=False)
        audio = _mapping(item.get("audio"), f"{path}.audio", required=False)
        roles_value = item.get("roles", [])
        roles = tuple(str(role) for role in _sequence(roles_value, f"{path}.roles"))
        tracks.append(
            _track_common(
                path=path,
                track_id=_string(item, "track_id", path),
                kind=_string(item, "kind", path),
                language=primary,
                timebase=track_tb,
                sample_count=_integer(item, "sample_total", path),
                duration_ticks=track_tb.ticks_from_units(_integer(item, "duration_us", path), 1_000_000, f"{path}.duration_us"),
                codec=_string(item, "codec", path),
                width=_optional_integer(video, "width", f"{path}.video") if video else None,
                height=_optional_integer(video, "height", f"{path}.video") if video else None,
                frame_interval_ticks=_optional_integer(video, "frame_interval_ticks", f"{path}.video") if video else None,
                sample_rate=_optional_integer(audio, "sample_rate", f"{path}.audio") if audio else None,
                channels=_optional_integer(audio, "channels", f"{path}.audio") if audio else None,
                sample_format=str(audio.get("sample_format", "s16le")) if audio else None,
                languages=tuple(str(language) for language in languages),
                roles=roles,
                hdr=item.get("hdr") if isinstance(item.get("hdr"), str) else None,
                custom=_tags(item.get("custom"), f"{path}.custom"),
            )
        )
    chapter_tb = (
        _timebase(root, "chapter_timebase", "$")
        if "chapter_timebase" in root
        else Timebase(1, 600)
    )
    chapters: list[Chapter] = []
    for position, value in enumerate(_sequence(root.get("chapter_table", []), "$.chapter_table")):
        path = f"$.chapter_table[{position}]"
        item = _mapping(value, path)
        start_ticks = chapter_tb.convert_ticks(_integer(item, "from_frame", path), timeline_tb, f"{path}.from_frame")
        end_ticks = chapter_tb.convert_ticks(_integer(item, "to_frame", path), timeline_tb, f"{path}.to_frame")
        chapters.append(
            Chapter(
                chapter_id=_string(item, "chapter_id", path),
                index=_integer(item, "number", path),
                start_ticks=start_ticks,
                end_ticks=end_ticks,
                title=str(item.get("title", "")),
                tags=_tags(item.get("tags"), f"{path}.tags"),
            )
        )
    resources = _mapping(root.get("resources"), "$.resources", required=False)
    cover, cover_mime = _cover_from_object(resources.get("cover"), "$.resources.cover")
    tags: dict[str, Any] = {}
    for position, value in enumerate(_sequence(root.get("custom_tags", []), "$.custom_tags")):
        path = f"$.custom_tags[{position}]"
        item = _mapping(value, path)
        tags[_string(item, "key", path)] = _required(item, "value", path)
    return ContainerModel(
        model_id=_string(root, "asset_id", "$"),
        title=_string(root, "display_title", "$"),
        source_version="v3",
        timebase=timeline_tb,
        duration_ticks=duration_ticks,
        tracks=tuple(tracks),
        chapters=tuple(sorted(chapters, key=lambda item: (item.start_ticks, item.end_ticks, item.index))),
        cover=cover,
        cover_mime=cover_mime,
        tags=tags,
        editorial=_tags(root.get("editorial"), "$.editorial"),
    )


def _version_rank(version: str) -> int:
    return int(version[1:])


def collect_losses(model: ContainerModel, target_version: str) -> tuple[LossyWarning, ...]:
    target = normalize_target_version(target_version)
    warnings: list[LossyWarning] = []
    target_rank = _version_rank(target)
    for track_index, track in enumerate(model.tracks):
        base = f"$.tracks[{track_index}]"
        if target_rank < 3:
            for language_index, language in enumerate(track.languages):
                if language == track.language:
                    continue
                warnings.append(
                    LossyWarning(
                        field_path=f"{base}.languages[{language_index}]",
                        message=f"{target} cannot express additional track languages",
                        source_value=language,
                        target_version=target,
                    )
                )
            if track.hdr:
                warnings.append(
                    LossyWarning(
                        field_path=f"{base}.hdr",
                        message=f"{target} cannot express HDR metadata",
                        source_value=track.hdr,
                        target_version=target,
                    )
                )
            for key in sorted(track.custom):
                warnings.append(
                    LossyWarning(
                        field_path=f"{base}.custom.{key}",
                        message=f"{target} cannot express per-track custom metadata",
                        source_value=track.custom[key],
                        target_version=target,
                    )
                )
        if target_rank < 2 and track.roles:
            warnings.append(
                LossyWarning(
                    field_path=f"{base}.roles",
                    message=f"{target} cannot express track roles",
                    source_value=list(track.roles),
                    target_version=target,
                )
            )
    if target_rank < 3:
        for chapter_index, chapter in enumerate(model.chapters):
            for key in sorted(chapter.tags):
                warnings.append(
                    LossyWarning(
                        field_path=f"$.chapters[{chapter_index}].tags.{key}",
                        message=f"{target} cannot express chapter custom tags",
                        source_value=chapter.tags[key],
                        target_version=target,
                    )
                )
        for key in sorted(model.editorial):
            warnings.append(
                LossyWarning(
                    field_path=f"$.editorial.{key}",
                    message=f"{target} cannot express editorial metadata",
                    source_value=model.editorial[key],
                    target_version=target,
                )
            )
    return tuple(warnings)


def project_model(model: ContainerModel, target_version: str) -> ContainerModel:
    target = normalize_target_version(target_version)
    rank = _version_rank(target)
    tracks = []
    for track in model.tracks:
        tracks.append(
            replace(
                track,
                payload=b"",
                languages=track.languages if rank >= 3 else (track.language,),
                roles=track.roles if rank >= 2 else (),
                hdr=track.hdr if rank >= 3 else None,
                custom=dict(track.custom) if rank >= 3 else {},
            )
        )
    chapters = tuple(
        sorted(
            (
                replace(chapter, tags=dict(chapter.tags) if rank >= 3 else {})
                for chapter in model.chapters
            ),
            key=lambda chapter: (chapter.start_ticks, chapter.end_ticks, chapter.index),
        )
    )
    return replace(
        model,
        source_version=target,
        tracks=tuple(tracks),
        chapters=chapters,
        editorial=dict(model.editorial) if rank >= 3 else {},
    )


def export_metadata(model: ContainerModel, target_version: str) -> MetadataExport:
    target = normalize_target_version(target_version)
    chapters = validate_chapters(model)
    warnings = collect_losses(model, target)
    trace: list[dict[str, Any]] = []
    if target == "v1":
        metadata = _export_v1(model, chapters, trace)
    elif target == "v2":
        metadata = _export_v2(model, chapters, trace)
    else:
        metadata = _export_v3(model, chapters, trace)
    trace.extend(
        {
            "code": warning.code,
            "source_path": warning.field_path,
            "target_version": target,
            "mapping": warning.mapping,
            "reason": warning.message,
        }
        for warning in warnings
    )
    return MetadataExport(
        target_version=target,
        metadata=metadata,
        warnings=warnings,
        trace=tuple(trace),
    )


def _export_v1(
    model: ContainerModel,
    chapters: tuple[Chapter, ...],
    trace: list[dict[str, Any]],
) -> dict[str, Any]:
    tracks: list[dict[str, Any]] = []
    for index, track in enumerate(model.tracks):
        track_path = f"$.tracks[{index}].duration_ms"
        item: dict[str, Any] = {
            "track_id": track.track_id,
            "type": track.kind,
            "language": track.language,
            "timebase": track.timebase.to_dict(),
            "duration_ms": track.timebase.to_units(track.duration_ticks, 1000, track_path),
            "sample_count": track.sample_count,
            "codec": track.codec,
        }
        if track.kind == "video":
            item["video"] = {
                "width": track.width,
                "height": track.height,
                "frame_interval_ticks": track.frame_interval_ticks,
            }
        else:
            item["audio"] = {
                "sample_rate": track.sample_rate,
                "channels": track.channels,
                "sample_format": track.sample_format or "s16le",
            }
        tracks.append(item)
    chapter_values = []
    for position, chapter in enumerate(chapters):
        chapter_values.append(
            {
                "chapter_id": chapter.chapter_id,
                "number": chapter.index,
                "start_ms": model.timebase.to_units(chapter.start_ticks, 1000, f"$.chapters[{position}].start_ms"),
                "end_ms": model.timebase.to_units(chapter.end_ticks, 1000, f"$.chapters[{position}].end_ms"),
                "label": chapter.title,
            }
        )
    trace.append(
        {
            "code": "E_MAP",
            "source_path": "$.tracks[*].timebase",
            "target_path": "$.tracks[*].timebase",
            "operation": "preserve",
        }
    )
    trace.append(
        {
            "code": "E_MAP",
            "source_path": "$.chapters[*].{start,end}_ticks",
            "target_path": "$.chapters[*].{start,end}_ms",
            "operation": "exact_rational_conversion",
            "residual": 0,
        }
    )
    return {
        "metadata_version": 1,
        "asset_id": model.model_id,
        "title": model.title,
        "timeline_timebase": model.timebase.to_dict(),
        "duration_ms": model.timebase.to_units(model.duration_ticks, 1000, "$.duration_ms"),
        "tracks": tracks,
        "chapters": chapter_values,
        "cover": {
            "mime": model.cover_mime,
            "encoding": "base64",
            "data": encode_base64(model.cover),
        }
        if model.cover
        else {},
        "tags": dict(sorted(model.tags.items())),
    }


def _export_v2(
    model: ContainerModel,
    chapters: tuple[Chapter, ...],
    trace: list[dict[str, Any]],
) -> dict[str, Any]:
    tracks: list[dict[str, Any]] = []
    for track in model.tracks:
        item: dict[str, Any] = {
            "track_id": track.track_id,
            "kind": track.kind,
            "language_tag": track.language,
            "timebase": track.timebase.to_dict(),
            "duration_ticks": track.duration_ticks,
            "sample_count": track.sample_count,
            "codec": track.codec,
        }
        if track.roles:
            item["roles"] = list(track.roles)
        if track.kind == "video":
            item["video"] = {
                "width": track.width,
                "height": track.height,
                "frame_interval_ticks": track.frame_interval_ticks,
            }
        else:
            item["audio"] = {
                "sample_rate": track.sample_rate,
                "channels": track.channels,
                "sample_format": track.sample_format or "s16le",
            }
        tracks.append(item)
    chapter_table: dict[str, Any] = {}
    for chapter in chapters:
        chapter_table[chapter.chapter_id] = {
            "chapter_id": chapter.chapter_id,
            "index": chapter.index,
            "start_ticks": chapter.start_ticks,
            "end_ticks": chapter.end_ticks,
            "name": chapter.title,
        }
    trace.append(
        {
            "code": "E_MAP",
            "source_path": "$.tracks[*].timebase",
            "target_path": "$.tracks[*].timebase",
            "operation": "preserve",
        }
    )
    trace.append(
        {
            "code": "E_MAP",
            "source_path": "$.chapters[*].{start,end}_ticks",
            "target_path": "$.chapters.*.{start,end}_ticks",
            "operation": "identity",
            "residual": 0,
        }
    )
    return {
        "metadata_version": 2,
        "asset": {
            "asset_id": model.model_id,
            "title": model.title,
        },
        "timeline": {
            "timebase": model.timebase.to_dict(),
            "duration_ticks": model.duration_ticks,
        },
        "tracks": tracks,
        "chapters": chapter_table,
        "assets": {
            "cover": {
                "mime": model.cover_mime,
                "base64": encode_base64(model.cover),
            }
        }
        if model.cover
        else {},
        "metadata": {
            "tags": dict(sorted(model.tags.items())),
        },
    }


def _export_v3(
    model: ContainerModel,
    chapters: tuple[Chapter, ...],
    trace: list[dict[str, Any]],
) -> dict[str, Any]:
    tracks: list[dict[str, Any]] = []
    for index, track in enumerate(model.tracks):
        languages = list(track.languages) or [track.language]
        if track.language not in languages:
            languages.insert(0, track.language)
        item: dict[str, Any] = {
            "track_id": track.track_id,
            "kind": track.kind,
            "primary_language": track.language,
            "languages": languages,
            "time_base": track.timebase.to_dict(),
            "duration_us": track.timebase.to_units(
                track.duration_ticks,
                1_000_000,
                f"$.tracks[{index}].duration_us",
            ),
            "sample_total": track.sample_count,
            "codec": track.codec,
        }
        if track.roles:
            item["roles"] = list(track.roles)
        if track.hdr:
            item["hdr"] = track.hdr
        if track.custom:
            item["custom"] = dict(sorted(track.custom.items()))
        if track.kind == "video":
            item["video"] = {
                "width": track.width,
                "height": track.height,
                "frame_interval_ticks": track.frame_interval_ticks,
            }
        else:
            item["audio"] = {
                "sample_rate": track.sample_rate,
                "channels": track.channels,
                "sample_format": track.sample_format or "s16le",
            }
        tracks.append(item)
    chapter_table = []
    for position, chapter in enumerate(chapters):
        chapter_table.append(
            {
                "chapter_id": chapter.chapter_id,
                "number": chapter.index,
                "from_frame": model.timebase.convert_ticks(
                    chapter.start_ticks,
                    CHAPTER_TIMEBASE,
                    f"$.chapters[{position}].from_frame",
                ),
                "to_frame": model.timebase.convert_ticks(
                    chapter.end_ticks,
                    CHAPTER_TIMEBASE,
                    f"$.chapters[{position}].to_frame",
                ),
                "title": chapter.title,
                "tags": dict(sorted(chapter.tags.items())),
            }
        )
    trace.append(
        {
            "code": "E_MAP",
            "source_path": "$.tracks[*].timebase",
            "target_path": "$.tracks[*].time_base",
            "operation": "preserve",
        }
    )
    trace.append(
        {
            "code": "E_MAP",
            "source_path": "$.chapters[*].{start,end}_ticks",
            "target_path": "$.chapter_table[*].{from,to}_frame",
            "operation": "exact_rational_conversion",
            "residual": 0,
        }
    )
    return {
        "metadata_version": 3,
        "asset_id": model.model_id,
        "display_title": model.title,
        "timeline": {
            "timebase": model.timebase.to_dict(),
            "duration_us": model.timebase.to_units(model.duration_ticks, 1_000_000, "$.timeline.duration_us"),
        },
        "chapter_timebase": CHAPTER_TIMEBASE.to_dict(),
        "tracks": tracks,
        "chapter_table": chapter_table,
        "resources": {
            "cover": {
                "mime": model.cover_mime,
                "base64": encode_base64(model.cover),
            }
        }
        if model.cover
        else {},
        "custom_tags": [
            {"key": key, "value": value, "scope": "public"}
            for key, value in sorted(model.tags.items())
        ],
        "editorial": dict(sorted(model.editorial.items())),
    }


def warnings_as_dicts(warnings: tuple[LossyWarning, ...]) -> list[dict[str, Any]]:
    return [warning.to_dict() for warning in warnings]
