from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from .errors import E_CONFLICT, E_CORRUPT, E_IO, E_META, E_ORDER, E_PREC, E_TRACK, WorkbenchError, fail
from .jsonutil import (
    canonical_json_bytes,
    decode_base64,
    read_json,
    sha256_bytes,
)
from .model import ContainerModel, Frame, MediaTrack, validate_chapters
from .timebase import Timebase
from .truth import TruthManifest, TruthTrack


@dataclass(frozen=True)
class RawStream:
    track_id: str
    kind: str
    language: str
    timebase: Timebase
    sample_count: int
    duration_ticks: int
    payload: bytes
    payload_encoding: str
    properties: dict[str, Any]

    @property
    def payload_sha256(self) -> str:
        return sha256_bytes(self.payload)


def _mapping(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        fail(E_TRACK, path, "raw stream must be an object")
    return value


def _sequence(value: Any, path: str) -> list[Any]:
    if not isinstance(value, list):
        fail(E_TRACK, path, "expected an array")
    return value


def _required(value: dict[str, Any], key: str, path: str) -> Any:
    if key not in value:
        fail(E_TRACK, f"{path}.{key}", "required raw stream field is missing")
    return value[key]


def _integer(value: dict[str, Any], key: str, path: str) -> int:
    item = _required(value, key, path)
    if isinstance(item, bool) or not isinstance(item, int):
        fail(E_TRACK, f"{path}.{key}", "expected an integer", measured=item)
    return item


def _string(value: dict[str, Any], key: str, path: str) -> str:
    item = _required(value, key, path)
    if not isinstance(item, str) or not item:
        fail(E_TRACK, f"{path}.{key}", "expected a non-empty string", measured=item)
    return item


def _timebase(value: dict[str, Any], key: str, path: str) -> Timebase:
    item = _required(value, key, path)
    if not isinstance(item, dict):
        fail(E_TRACK, f"{path}.{key}", "timebase must be an object")
    try:
        return Timebase.from_dict(item, f"{path}.{key}")
    except Exception as exc:
        fail(E_TRACK, f"{path}.{key}", f"invalid timebase: {exc}")


def load_video_stream(value: Any, *, source_path: str = "$.video") -> RawStream:
    root = _mapping(value, source_path)
    kind = _string(root, "kind", source_path)
    if kind != "video":
        fail(E_TRACK, f"{source_path}.kind", "video stream has an invalid kind", measured=kind)
    timebase = _timebase(root, "timebase", source_path)
    interval = _integer(root, "frame_interval_ticks", source_path)
    if interval <= 0:
        fail(E_TRACK, f"{source_path}.frame_interval_ticks", "frame interval must be positive")
    rows = _integer(root, "rows", source_path)
    cols = _integer(root, "cols", source_path)
    if rows <= 0 or cols <= 0:
        fail(E_TRACK, f"{source_path}.rows", "video grid dimensions must be positive")
    frames: list[Frame] = []
    raw_frames = _sequence(_required(root, "frames", source_path), f"{source_path}.frames")
    for index, frame_value in enumerate(raw_frames):
        frame_path = f"{source_path}.frames[{index}]"
        item = _mapping(frame_value, frame_path)
        pts = _integer(item, "pts_ticks", frame_path)
        frame_rows = _integer(item, "rows", frame_path)
        frame_cols = _integer(item, "cols", frame_path)
        cells = _sequence(_required(item, "cells", frame_path), f"{frame_path}.cells")
        if len(cells) != frame_rows * frame_cols:
            fail(
                E_TRACK,
                f"{frame_path}.cells",
                "video frame cell count does not match its grid",
                measured=len(cells),
                declared=frame_rows * frame_cols,
            )
        frame = Frame.from_values(
            pts_ticks=pts,
            rows=frame_rows,
            cols=frame_cols,
            cells=(str(cell) for cell in cells),
        )
        declared_hash = _required(item, "sha256", frame_path)
        if not isinstance(declared_hash, str) or declared_hash != frame.sha256:
            fail(
                E_CORRUPT,
                f"{frame_path}.sha256",
                "video frame hash mismatch",
                measured=frame.sha256,
                declared=declared_hash,
            )
        if frame_rows != rows or frame_cols != cols:
            fail(
                E_CONFLICT,
                f"{frame_path}",
                "frame grid dimensions conflict with the track dimensions",
                measured={"rows": frame_rows, "cols": frame_cols},
                declared={"rows": rows, "cols": cols},
            )
        frames.append(frame)
    if not frames:
        fail(E_TRACK, f"{source_path}.frames", "video stream has no frames")
    for index in range(1, len(frames)):
        delta = frames[index].pts_ticks - frames[index - 1].pts_ticks
        path = f"{source_path}.frames[{index}].pts_ticks"
        if delta <= 0:
            fail(
                E_ORDER,
                path,
                "video timestamps must be strictly increasing",
                measured=frames[index].pts_ticks,
                declared={"previous_pts_ticks": frames[index - 1].pts_ticks},
            )
        if delta % interval != 0:
            fail(
                E_ORDER,
                path,
                "video timestamp interval is not an integer multiple of the declared base unit",
                measured=delta,
                declared={"frame_interval_ticks": interval},
            )
    language = str(root.get("language", ""))
    payload = canonical_json_bytes([frame.descriptor() for frame in frames])
    sample_count = len(frames)
    declared_count = root.get("frame_count")
    if declared_count is not None and declared_count != sample_count:
        fail(
            E_TRACK,
            f"{source_path}.frame_count",
            "declared video frame count does not match the frame array",
            measured=sample_count,
            declared=declared_count,
        )
    duration_ticks = sample_count * interval
    return RawStream(
        track_id=_string(root, "track_id", source_path),
        kind="video",
        language=language,
        timebase=timebase,
        sample_count=sample_count,
        duration_ticks=duration_ticks,
        payload=payload,
        payload_encoding="json.frames.v1",
        properties={
            "width": _integer(root, "width", source_path),
            "height": _integer(root, "height", source_path),
            "frame_interval_ticks": interval,
            "codec": str(root.get("codec", "grid-rgb-v1")),
        },
    )


def load_audio_stream(value: Any, *, source_path: str = "$.audio") -> RawStream:
    root = _mapping(value, source_path)
    kind = _string(root, "kind", source_path)
    if kind != "audio":
        fail(E_TRACK, f"{source_path}.kind", "audio stream has an invalid kind", measured=kind)
    timebase = _timebase(root, "timebase", source_path)
    sample_rate = _integer(root, "sample_rate", source_path)
    channels = _integer(root, "channels", source_path)
    sample_format = str(root.get("sample_format", "s16le"))
    if sample_rate <= 0 or channels <= 0:
        fail(E_TRACK, f"{source_path}", "audio sample rate and channels must be positive")
    if sample_format != "s16le":
        fail(E_TRACK, f"{source_path}.sample_format", "only s16le audio is supported", measured=sample_format)
    if timebase.ticks_per_second != sample_rate:
        fail(
            E_TRACK,
            f"{source_path}.timebase",
            "audio timebase must exactly match the sample rate",
            measured=timebase.to_dict(),
            declared={"sample_rate": sample_rate},
        )
    encoded = _required(root, "samples_base64", source_path)
    if not isinstance(encoded, str):
        fail(E_TRACK, f"{source_path}.samples_base64", "audio samples must be base64 text")
    try:
        payload = decode_base64(encoded)
    except (ValueError, TypeError) as exc:
        fail(E_CORRUPT, f"{source_path}.samples_base64", f"invalid audio sample encoding: {exc}")
    bytes_per_sample = 2 * channels
    if len(payload) % bytes_per_sample != 0:
        fail(
            E_CORRUPT,
            f"{source_path}.samples_base64",
            "audio payload is not aligned to the sample frame size",
            measured=len(payload),
            declared={"bytes_per_frame": bytes_per_sample},
        )
    sample_count = len(payload) // bytes_per_sample
    declared_count = root.get("sample_count")
    if declared_count is not None and declared_count != sample_count:
        fail(
            E_TRACK,
            f"{source_path}.sample_count",
            "declared audio sample count does not match the payload",
            measured=sample_count,
            declared=declared_count,
        )
    declared_hash = root.get("samples_sha256")
    measured_hash = sha256_bytes(payload)
    if declared_hash is not None and declared_hash != measured_hash:
        fail(
            E_CORRUPT,
            f"{source_path}.samples_sha256",
            "audio sample hash mismatch",
            measured=measured_hash,
            declared=declared_hash,
        )
    try:
        duration_ticks = timebase.ticks_from_seconds(
            Fraction(sample_count, sample_rate),
            f"{source_path}.duration_ticks",
        )
    except WorkbenchError as exc:
        if exc.code == E_PREC:
            fail(
                E_TRACK,
                f"{source_path}.timebase",
                "audio sample duration cannot be represented in the declared timebase",
                measured=timebase.to_dict(),
                declared={"sample_rate": sample_rate, "sample_count": sample_count},
            )
        raise
    return RawStream(
        track_id=_string(root, "track_id", source_path),
        kind="audio",
        language=str(root.get("language", "")),
        timebase=timebase,
        sample_count=sample_count,
        duration_ticks=duration_ticks,
        payload=payload,
        payload_encoding="pcm.s16le",
        properties={
            "sample_rate": sample_rate,
            "channels": channels,
            "sample_format": sample_format,
            "codec": str(root.get("codec", "pcm-s16le")),
        },
    )


def load_raw_file(path: str | Path) -> RawStream:
    try:
        value = read_json(path)
    except OSError as exc:
        fail(E_IO, "$", f"cannot read raw stream: {exc}")
    except ValueError as exc:
        fail(E_CORRUPT, "$", f"raw stream is not valid JSON: {exc}")
    root = _mapping(value, "$")
    kind = root.get("kind")
    if kind == "video":
        return load_video_stream(root, source_path="$")
    if kind == "audio":
        return load_audio_stream(root, source_path="$")
    fail(E_TRACK, "$.kind", "raw stream kind must be video or audio", measured=kind)


def load_raw_directory(path: str | Path) -> dict[str, RawStream]:
    directory = Path(path)
    if not directory.exists() or not directory.is_dir():
        fail(E_TRACK, "$.raw", f"raw stream directory does not exist: {directory}")
    streams: dict[str, RawStream] = {}
    for file_path in sorted(directory.glob("*.json")):
        stream = load_raw_file(file_path)
        if stream.track_id in streams:
            fail(E_CONFLICT, f"$.tracks.{stream.track_id}", "duplicate raw track id", measured=stream.track_id)
        streams[stream.track_id] = stream
    if not streams:
        fail(E_TRACK, "$.raw", "raw stream directory contains no JSON tracks")
    return streams


def validate_raw_stream(stream: RawStream, path: str) -> None:
    if stream.sample_count <= 0:
        fail(E_TRACK, f"{path}.sample_count", "raw stream has no samples")
    if stream.kind == "video":
        interval = int(stream.properties["frame_interval_ticks"])
        try:
            frames = json.loads(stream.payload.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            fail(E_CORRUPT, f"{path}.payload", f"invalid video payload: {exc}")
        previous_pts: int | None = None
        for index, frame in enumerate(frames):
            pts = int(frame["pts_ticks"])
            if previous_pts is not None:
                delta = pts - previous_pts
                if delta <= 0:
                    fail(
                        E_ORDER,
                        f"{path}.frames[{index}].pts_ticks",
                        "video timestamps must be strictly increasing",
                        measured=pts,
                        declared={"previous_pts_ticks": previous_pts},
                    )
                if delta % interval != 0:
                    fail(
                        E_ORDER,
                        f"{path}.frames[{index}].pts_ticks",
                        "video timestamp interval is not aligned to the base unit",
                        measured=delta,
                        declared={"frame_interval_ticks": interval},
                    )
            declared_hash = frame.get("sha256")
            frame_body = {key: frame[key] for key in ("pts_ticks", "rows", "cols", "cells")}
            expected_hash = sha256_bytes(canonical_json_bytes(frame_body))
            if declared_hash != expected_hash:
                fail(
                    E_CORRUPT,
                    f"{path}.frames[{index}].sha256",
                    "video frame hash mismatch",
                    measured=expected_hash,
                    declared=declared_hash,
                )
            previous_pts = pts
        if len(frames) != stream.sample_count:
            fail(
                E_TRACK,
                f"{path}.sample_count",
                "video frame count does not match the declared sample count",
                measured=len(frames),
                declared=stream.sample_count,
            )
    elif len(stream.payload) != stream.sample_count * 2 * int(stream.properties["channels"]):
        fail(
            E_CORRUPT,
            f"{path}.payload",
            "audio payload length does not match the declared sample count",
            measured=len(stream.payload),
            declared=stream.sample_count * 2 * int(stream.properties["channels"]),
        )


def _truth_tracks_by_id(truth: TruthManifest) -> dict[str, TruthTrack]:
    return {track.track_id: track for track in truth.tracks}


def merge_raw_streams(
    model: ContainerModel,
    streams: dict[str, RawStream],
    *,
    truth: TruthManifest | None = None,
) -> ContainerModel:
    tracks_by_id = {track.track_id: track for track in model.tracks}
    for extra_id in sorted(set(streams) - set(tracks_by_id)):
        fail(E_TRACK, f"$.tracks.{extra_id}", "raw track is not declared by metadata", measured=extra_id)
    merged: list[MediaTrack] = []
    for index, track in enumerate(model.tracks):
        path = f"$.tracks[{index}]"
        stream = streams.get(track.track_id)
        if stream is None:
            fail(
                E_TRACK,
                path,
                "metadata track is missing from the raw stream set",
                measured=sorted(streams),
                declared=track.track_id,
            )
        validate_raw_stream(stream, path)
        if stream.kind != track.kind:
            fail(
                E_CONFLICT,
                f"{path}.kind",
                "metadata and raw stream track kinds conflict",
                measured=track.kind,
                declared=stream.kind,
            )
        if stream.language and stream.language != track.language:
            fail(
                E_CONFLICT,
                f"{path}.language",
                "metadata and raw stream language tags conflict",
                measured=track.language,
                declared=stream.language,
            )
        if stream.timebase != track.timebase:
            fail(
                E_TRACK,
                f"{path}.timebase",
                "metadata and raw stream timebases do not match",
                measured=track.timebase.to_dict(),
                declared=stream.timebase.to_dict(),
            )
        if stream.sample_count != track.sample_count:
            fail(
                E_TRACK,
                f"{path}.sample_count",
                "metadata and raw stream sample counts do not match",
                measured=track.sample_count,
                declared=stream.sample_count,
            )
        if stream.duration_ticks != track.duration_ticks:
            fail(
                E_CONFLICT,
                f"{path}.duration_ticks",
                "metadata and raw stream durations do not match",
                measured=track.duration_ticks,
                declared=stream.duration_ticks,
            )
        properties = stream.properties
        for key in ("width", "height", "frame_interval_ticks", "sample_rate", "channels", "sample_format"):
            declared = getattr(track, key)
            measured = properties.get(key)
            if declared is not None and measured is not None and declared != measured:
                fail(
                    E_CONFLICT,
                    f"{path}.{key}",
                    "metadata and raw stream properties conflict",
                    measured=declared,
                    declared=measured,
                )
        merged.append(track.with_payload(stream.payload, stream.payload_encoding))
    result = model.copy(tracks=tuple(merged))
    validate_chapters(result)
    if truth is not None:
        _verify_truth(result, truth)
    return result


def _verify_truth(model: ContainerModel, truth: TruthManifest) -> None:
    if model.model_id != truth.sample_id:
        fail(
            E_CONFLICT,
            "$.model_id",
            "model id does not match the truth manifest",
            measured=model.model_id,
            declared=truth.sample_id,
        )
    if model.timebase != truth.timebase:
        fail(
            E_CONFLICT,
            "$.timebase",
            "model timebase does not match the truth manifest",
            measured=model.timebase.to_dict(),
            declared=truth.timebase.to_dict(),
        )
    if model.duration_ticks != truth.duration_ticks:
        fail(
            E_CONFLICT,
            "$.duration_ticks",
            "model duration does not match the truth manifest",
            measured=model.duration_ticks,
            declared=truth.duration_ticks,
        )
    expected_tracks = _truth_tracks_by_id(truth)
    actual_tracks = {track.track_id: track for track in model.tracks}
    for track_id in sorted(set(expected_tracks) - set(actual_tracks)):
        fail(E_TRACK, f"$.tracks.{track_id}", "truth track is absent from the imported model")
    for track_id in sorted(set(actual_tracks) - set(expected_tracks)):
        fail(E_TRACK, f"$.tracks.{track_id}", "imported track is absent from the truth manifest")
    for index, track in enumerate(model.tracks):
        expected = expected_tracks[track.track_id]
        if track.sample_count != expected.sample_count:
            fail(
                E_TRACK,
                f"$.tracks[{index}].sample_count",
                "track count does not match the truth manifest",
                measured=track.sample_count,
                declared=expected.sample_count,
            )
        if track.timebase != expected.timebase:
            fail(
                E_TRACK,
                f"$.tracks[{index}].timebase",
                "track timebase does not match the truth manifest",
                measured=track.timebase.to_dict(),
                declared=expected.timebase.to_dict(),
            )
        if track.payload_sha256 != expected.payload_sha256:
            fail(
                E_CORRUPT,
                f"$.tracks[{index}].payload_sha256",
                "track payload hash does not match the truth manifest",
                measured=track.payload_sha256,
                declared=expected.payload_sha256,
            )
        if expected.language and track.language != expected.language:
            fail(
                E_CONFLICT,
                f"$.tracks[{index}].language",
                "track language does not match the truth manifest",
                measured=track.language,
                declared=expected.language,
            )
    if model.cover and sha256_bytes(model.cover) != truth.cover_sha256:
        fail(
            E_CONFLICT,
            "$.cover",
            "cover hash does not match the truth manifest",
            measured=sha256_bytes(model.cover),
            declared=truth.cover_sha256,
        )
    for key in sorted(set(truth.tags) | set(model.tags)):
        if model.tags.get(key) != truth.tags.get(key):
            fail(
                E_CONFLICT,
                f"$.tags.{key}",
                "custom tag does not match the truth manifest",
                measured=model.tags.get(key),
                declared=truth.tags.get(key),
            )
    expected_chapters = {chapter.chapter_id: chapter for chapter in truth.chapters}
    actual_chapters = {chapter.chapter_id: chapter for chapter in model.chapters}
    for chapter_id in sorted(set(expected_chapters) | set(actual_chapters)):
        expected = expected_chapters.get(chapter_id)
        actual = actual_chapters.get(chapter_id)
        if actual is None:
            fail(E_CONFLICT, f"$.chapters.{chapter_id}", "truth chapter is absent from the model")
        if expected is None:
            fail(E_CONFLICT, f"$.chapters.{chapter_id}", "chapter is absent from the truth manifest")
        if actual.start_ticks != expected.start_ticks or actual.end_ticks != expected.end_ticks:
            fail(
                E_CONFLICT,
                f"$.chapters.{chapter_id}",
                "chapter interval does not match the truth manifest",
                measured={"start": actual.start_ticks, "end": actual.end_ticks},
                declared={"start": expected.start_ticks, "end": expected.end_ticks},
            )
