from __future__ import annotations

import base64
import json
from collections.abc import Iterable
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

import numpy as np

from .engine import (
    BuildState,
    SegmentSignal,
    apply_limiter,
    build_segment_signal,
    segment_sample_interval,
    segment_signal_hash,
)
from .errors import E_RECOVERY_INVALID, HapticError
from .model import ProjectConfig, WaveSegment, parse_project
from .util import (
    GENESIS_HASH,
    atomic_write_bytes,
    canonical_json_bytes,
    merge_ranges,
)
from .versioning import (
    ChunkDigest,
    Publisher,
    SegmentRecord,
    VersionedSignal,
    VersionHistoryEntry,
    chunk_signal_hash,
    compute_content_hash,
)

SNAPSHOT_SCHEMA = "haptic-snapshot-v1"


@dataclass(frozen=True)
class RecoveryResult:
    state: BuildState
    history: tuple[VersionHistoryEntry, ...]
    recomputed_chunk_ranges: tuple[tuple[int, int], ...]
    recomputed_segment_indices: tuple[int, ...]
    source_path: Path | None = None

    @property
    def recomputed_chunks(self) -> int:
        return len(self.recomputed_chunk_ranges)

    def publisher(self) -> Publisher:
        return Publisher(initial=self.state.version, history=self.history)


def _invalid(message: str, **details: Any) -> HapticError:
    return HapticError(
        E_RECOVERY_INVALID,
        message,
        details=details or None,
    )


def _encode_f64(values: np.ndarray) -> str:
    data = np.asarray(values, dtype="<f8", order="C").tobytes()
    return base64.b64encode(data).decode("ascii")


def _decode_f64(encoded: Any, expected_length: int, field: str) -> np.ndarray | None:
    if not isinstance(encoded, str):
        return None
    try:
        raw = base64.b64decode(encoded.encode("ascii"), validate=True)
    except (ValueError, UnicodeError):
        return None
    if len(raw) != expected_length * 8:
        return None
    return np.frombuffer(raw, dtype="<f8").astype(np.float64, copy=True)


def _encode_mask(values: np.ndarray) -> str:
    packed = np.packbits(np.asarray(values, dtype=np.uint8), bitorder="big")
    return base64.b64encode(packed.tobytes()).decode("ascii")


def _decode_mask(encoded: Any, expected_length: int) -> np.ndarray | None:
    if not isinstance(encoded, str):
        return None
    try:
        raw = base64.b64decode(encoded.encode("ascii"), validate=True)
    except (ValueError, UnicodeError):
        return None
    expected_bytes = (expected_length + 7) // 8
    if len(raw) != expected_bytes:
        return None
    unpacked = np.unpackbits(
        np.frombuffer(raw, dtype=np.uint8),
        bitorder="big",
    )
    return unpacked[:expected_length].astype(np.bool_)


def _recompute_chunk(
    start: int,
    end: int,
    config: ProjectConfig,
    segment_signals: Iterable[SegmentSignal],
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    raw_left = np.zeros(end - start, dtype=np.float64)
    raw_right = np.zeros(end - start, dtype=np.float64)
    for signal in sorted(
        segment_signals,
        key=lambda item: (item.start_sample, item.index),
    ):
        overlap_start = max(start, signal.start_sample)
        overlap_end = min(end, signal.end_sample)
        if overlap_start >= overlap_end:
            continue
        source_start = overlap_start - signal.start_sample
        source_end = source_start + (overlap_end - overlap_start)
        target_start = overlap_start - start
        target_end = target_start + (overlap_end - overlap_start)
        raw_left[target_start:target_end] += signal.left[source_start:source_end]
        raw_right[target_start:target_end] += signal.right[source_start:source_end]
    left, limited_l, out_l = apply_limiter(raw_left, config)
    right, limited_r, out_r = apply_limiter(raw_right, config)
    return left, right, limited_l | limited_r, out_l | out_r


class SnapshotManager:
    @staticmethod
    def save(
        path: str | Path,
        publisher: Publisher,
        state: BuildState,
        *,
        incomplete_chunk_starts: Iterable[int] = (),
    ) -> Path:
        version = publisher.require_current()
        if state.version is not version and (
            state.version.content_hash != version.content_hash
            or state.version.version != version.version
        ):
            raise _invalid("build state does not match published version")

        incomplete = {int(start) for start in incomplete_chunk_starts}
        signal_map = state.segment_signal_map()
        segment_records: list[dict[str, Any]] = []
        for record in version.segment_records:
            signal = signal_map.get(record.index)
            if signal is None:
                raise _invalid(
                    "segment cache is missing during snapshot",
                    segment_index=record.index,
                )
            segment_records.append(
                {
                    **record.to_dict(),
                    "complete": True,
                    "left_b64": _encode_f64(signal.left),
                    "right_b64": _encode_f64(signal.right),
                }
            )

        chunks: list[dict[str, Any]] = []
        for digest in version.chunks:
            start = digest.start_sample
            end = digest.end_sample
            complete = start not in incomplete
            chunk: dict[str, Any] = {
                **digest.to_dict(),
                "complete": complete,
            }
            if complete:
                chunk.update(
                    {
                        "left_b64": _encode_f64(version.left[start:end]),
                        "right_b64": _encode_f64(version.right[start:end]),
                        "limited_mask_b64": _encode_mask(
                            version.limited_mask[start:end]
                        ),
                        "out_of_range_mask_b64": _encode_mask(
                            version.out_of_range_mask[start:end]
                        ),
                    }
                )
            chunks.append(chunk)

        payload = {
            "schema": SNAPSHOT_SCHEMA,
            "saved_version": version.version,
            "history": [entry.to_dict() for entry in publisher.history()],
            "project": {
                "config": version.config.to_dict(),
                "segments": [segment.to_dict() for segment in version.segments],
            },
            "published": {
                "version": version.version,
                "parent_hash": version.parent_hash,
                "content_hash": version.content_hash,
                "sample_count": version.sample_count,
                "limited_samples": version.limited_samples,
                "out_of_range_samples": version.out_of_range_samples,
                "modified_indices": list(version.modified_indices),
                "segment_records": segment_records,
                "chunks": chunks,
            },
        }
        target = Path(path)
        atomic_write_bytes(
            target,
            canonical_json_bytes(payload) + b"\n",
        )
        return target

    @staticmethod
    def load(path: str | Path) -> RecoveryResult:
        source = Path(path)
        try:
            raw = source.read_text(encoding="utf-8")
        except OSError as exc:
            raise _invalid(f"cannot read snapshot {source}: {exc}") from exc
        try:
            payload = json.loads(raw)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise _invalid(f"snapshot JSON is invalid: {exc}") from exc
        try:
            result = SnapshotManager._load_payload(payload)
            return replace(result, source_path=source)
        except HapticError:
            raise
        except Exception as exc:
            raise _invalid(f"snapshot recovery failed: {exc}") from exc

    @staticmethod
    def load_latest(
        directory: str | Path,
        *,
        pattern: str = "snapshot*.json",
    ) -> tuple[Path, RecoveryResult]:
        """Load the highest-version snapshot that passes every integrity check."""
        base = Path(directory)
        if not base.is_dir():
            raise _invalid(f"snapshot directory does not exist: {base}")
        best_path: Path | None = None
        best_result: RecoveryResult | None = None
        failures: list[dict[str, Any]] = []
        for path in sorted(base.glob(pattern)):
            if not path.is_file() or path.name.startswith("."):
                continue
            try:
                result = SnapshotManager.load(path)
            except HapticError as exc:
                failures.append(
                    {
                        "file": path.name,
                        "code": exc.code,
                        "message": exc.message,
                    }
                )
                continue
            if (
                best_result is None
                or result.state.version.version > best_result.state.version.version
            ):
                best_path = path
                best_result = result
        if best_result is None or best_path is None:
            raise _invalid(
                "no valid snapshot found",
                directory=str(base),
                failures=failures,
            )
        return best_path, best_result

    @staticmethod
    def _load_payload(payload: Any) -> RecoveryResult:
        if not isinstance(payload, dict) or payload.get("schema") != SNAPSHOT_SCHEMA:
            raise _invalid("unsupported or missing snapshot schema")
        saved_version = payload.get("saved_version")
        if not isinstance(saved_version, int) or saved_version < 1:
            raise _invalid("saved_version is missing or invalid")

        history_raw = payload.get("history")
        if not isinstance(history_raw, list) or not history_raw:
            raise _invalid("version history is missing")
        history: list[VersionHistoryEntry] = []
        previous_hash: str | None = None
        for expected_version, entry in enumerate(history_raw, start=1):
            if not isinstance(entry, dict):
                raise _invalid("history entry must be an object")
            for field in ("version", "parent_hash", "content_hash"):
                if field not in entry:
                    raise _invalid(f"history entry is missing {field}")
            if entry["version"] != expected_version:
                raise _invalid("version history is not contiguous")
            parent_hash = entry["parent_hash"]
            content_hash = entry["content_hash"]
            if not isinstance(parent_hash, str) or len(parent_hash) != 64:
                raise _invalid("history parent hash is malformed")
            if not isinstance(content_hash, str) or len(content_hash) != 64:
                raise _invalid("history content hash is malformed")
            if previous_hash is None:
                if parent_hash != GENESIS_HASH:
                    raise _invalid("version history does not start at genesis")
            elif parent_hash != previous_hash:
                raise _invalid("version hash chain is broken")
            history.append(
                VersionHistoryEntry(
                    version=expected_version,
                    parent_hash=parent_hash,
                    content_hash=content_hash,
                )
            )
            previous_hash = content_hash

        published = payload.get("published")
        if not isinstance(published, dict):
            raise _invalid("published version is missing")
        if published.get("version") != saved_version:
            raise _invalid("published version differs from saved_version")
        if history[-1].version != saved_version:
            raise _invalid("latest history version differs from saved_version")
        if history[-1].content_hash != published.get("content_hash"):
            raise _invalid("latest history hash differs from published hash")
        if history[-1].parent_hash != published.get("parent_hash"):
            raise _invalid("latest parent hash differs from published parent")

        project_raw = payload.get("project")
        if not isinstance(project_raw, dict):
            raise _invalid("project data is missing")
        config_raw = project_raw.get("config")
        segments_raw = project_raw.get("segments")
        if not isinstance(config_raw, dict) or not isinstance(segments_raw, list):
            raise _invalid("project config or segments are missing")
        try:
            project = parse_project(
                {
                    **config_raw,
                    "segments": segments_raw,
                }
            )
        except HapticError as exc:
            raise _invalid(
                "snapshot project cannot be parsed",
                cause=exc.to_dict(),
            ) from exc
        config = project.config
        segments = project.segments
        if [segment.to_dict() for segment in segments] != segments_raw:
            raise _invalid("snapshot segment fields do not round-trip")

        sample_count = published.get("sample_count")
        if (
            not isinstance(sample_count, int)
            or sample_count < 0
            or sample_count != _expected_sample_count(segments, config.sample_rate)
        ):
            raise _invalid("published sample_count is inconsistent")

        segment_records_raw = published.get("segment_records")
        if not isinstance(segment_records_raw, list):
            raise _invalid("segment records are missing")
        segment_records = _load_segment_records(
            segment_records_raw,
            segments,
            config,
        )
        segment_signals = _load_segment_signals(
            segment_records_raw,
            segments,
            config,
            segment_records,
        )

        chunks_raw = published.get("chunks")
        if not isinstance(chunks_raw, list):
            raise _invalid("chunk records are missing")
        chunk_digests = _parse_chunk_metadata(
            chunks_raw,
            sample_count,
            config.chunk_size,
        )
        (
            left,
            right,
            limited_mask,
            out_of_range_mask,
            recomputed_chunk_ranges,
        ) = _load_chunk_payloads(
            chunks_raw,
            chunk_digests,
            sample_count,
            config,
            segment_signals,
        )

        limited_samples = sum(digest.limited_samples for digest in chunk_digests)
        out_of_range_samples = sum(
            digest.out_of_range_samples for digest in chunk_digests
        )
        if limited_samples != int(limited_mask.sum()):
            raise _invalid("limited-sample count does not match masks")
        if out_of_range_samples != int(out_of_range_mask.sum()):
            raise _invalid("out-of-range count does not match masks")
        if published.get("limited_samples") != limited_samples:
            raise _invalid("published limited-sample total is inconsistent")
        if published.get("out_of_range_samples") != out_of_range_samples:
            raise _invalid("published out-of-range total is inconsistent")

        expected_content_hash = compute_content_hash(
            version=saved_version,
            parent_hash=history[-1].parent_hash,
            config=config,
            segments=segments,
            segment_records=segment_records,
            chunks=chunk_digests,
            limited_samples=limited_samples,
            out_of_range_samples=out_of_range_samples,
        )
        if expected_content_hash != published.get("content_hash"):
            raise _invalid("latest content hash cannot be verified")

        modified_raw = published.get("modified_indices", [])
        if not isinstance(modified_raw, list) or not all(
            isinstance(index, int) for index in modified_raw
        ):
            raise _invalid("modified_indices is malformed")
        version = VersionedSignal(
            version=saved_version,
            parent_hash=history[-1].parent_hash,
            content_hash=published["content_hash"],
            sample_rate=config.sample_rate,
            left=left,
            right=right,
            limited_mask=limited_mask,
            out_of_range_mask=out_of_range_mask,
            segments=segments,
            config=config,
            segment_records=segment_records,
            chunks=chunk_digests,
            limited_samples=limited_samples,
            out_of_range_samples=out_of_range_samples,
            modified_indices=tuple(modified_raw),
        )
        state = BuildState(version=version, segment_signals=segment_signals)
        return RecoveryResult(
            state=state,
            history=tuple(history),
            recomputed_chunk_ranges=merge_ranges(
                recomputed_chunk_ranges,
                limit=sample_count,
            ),
            recomputed_segment_indices=_recomputed_segment_indices(segment_records_raw),
        )


def _expected_sample_count(
    segments: Iterable[WaveSegment],
    sample_rate: int,
) -> int:
    total = 0
    for segment in segments:
        _, end = segment_sample_interval(segment, sample_rate)
        total = max(total, end)
    return total


def _load_segment_records(
    records_raw: list[Any],
    segments: tuple[WaveSegment, ...],
    config: ProjectConfig,
) -> tuple[SegmentRecord, ...]:
    by_index: dict[int, dict[str, Any]] = {}
    for raw in records_raw:
        if not isinstance(raw, dict) or not isinstance(raw.get("index"), int):
            raise _invalid("segment record is missing an index")
        index = raw["index"]
        if index in by_index:
            raise _invalid("duplicate segment record", segment_index=index)
        by_index[index] = raw
    if set(by_index) != set(range(len(segments))):
        raise _invalid("segment records do not match the project")

    records: list[SegmentRecord] = []
    for segment in segments:
        raw = by_index[segment.index]
        interval = raw.get("sample_interval")
        sample_hash = raw.get("sample_hash")
        if (
            not isinstance(interval, list)
            or len(interval) != 2
            or not all(isinstance(value, int) for value in interval)
            or not isinstance(sample_hash, str)
        ):
            raise _invalid(
                "segment record fields are missing",
                segment_index=segment.index,
            )
        expected_interval = list(segment_sample_interval(segment, config.sample_rate))
        if interval != expected_interval:
            raise _invalid(
                "segment sample interval is inconsistent",
                segment_index=segment.index,
                stored=interval,
                expected=expected_interval,
            )
        records.append(
            SegmentRecord(
                index=segment.index,
                start_sample=interval[0],
                end_sample=interval[1],
                sample_hash=sample_hash,
            )
        )
    return tuple(records)


def _recomputed_segment_indices(
    records_raw: list[dict[str, Any]],
) -> tuple[int, ...]:
    return tuple(
        sorted(raw["index"] for raw in records_raw if raw.get("complete") is not True)
    )


def _load_segment_signals(
    records_raw: list[dict[str, Any]],
    segments: tuple[WaveSegment, ...],
    config: ProjectConfig,
    records: tuple[SegmentRecord, ...],
) -> tuple[SegmentSignal, ...]:
    raw_by_index = {raw["index"]: raw for raw in records_raw}
    record_by_index = {record.index: record for record in records}
    signals: list[SegmentSignal] = []
    for segment in segments:
        record = record_by_index[segment.index]
        raw = raw_by_index[segment.index]
        count = record.end_sample - record.start_sample
        left = _decode_f64(raw.get("left_b64"), count, "left_b64")
        right = _decode_f64(raw.get("right_b64"), count, "right_b64")
        valid = raw.get("complete") is True and left is not None and right is not None
        sample_hash = None
        if valid:
            sample_hash = segment_signal_hash(
                segment,
                record.start_sample,
                record.end_sample,
                left,
                right,
            )
            valid = sample_hash == record.sample_hash
        if valid:
            signals.append(
                SegmentSignal(
                    index=segment.index,
                    start_sample=record.start_sample,
                    end_sample=record.end_sample,
                    segment=segment,
                    left=left,
                    right=right,
                    sample_hash=sample_hash,
                )
            )
            continue
        rebuilt = build_segment_signal(segment, config)
        if rebuilt.sample_hash != record.sample_hash:
            raise _invalid(
                "segment payload cannot be verified or rebuilt",
                segment_index=segment.index,
            )
        signals.append(rebuilt)
    return tuple(signals)


def _parse_chunk_metadata(
    chunks_raw: list[Any],
    sample_count: int,
    chunk_size: int,
) -> tuple[ChunkDigest, ...]:
    chunks: list[ChunkDigest] = []
    cursor = 0
    for raw in chunks_raw:
        if not isinstance(raw, dict):
            raise _invalid("chunk record must be an object")
        interval = raw.get("sample_interval")
        signal_hash = raw.get("signal_hash")
        limited_samples = raw.get("limited_samples")
        out_of_range_samples = raw.get("out_of_range_samples")
        if (
            not isinstance(interval, list)
            or len(interval) != 2
            or not all(isinstance(value, int) for value in interval)
            or not isinstance(signal_hash, str)
            or not isinstance(limited_samples, int)
            or not isinstance(out_of_range_samples, int)
        ):
            raise _invalid("chunk metadata fields are missing")
        start, end = interval
        expected_end = min(sample_count, cursor + chunk_size)
        if start != cursor or end != expected_end or end <= start:
            raise _invalid(
                "chunk intervals are not contiguous",
                start=start,
                end=end,
            )
        length = end - start
        if not 0 <= limited_samples <= length:
            raise _invalid("chunk limited count is out of range")
        if not 0 <= out_of_range_samples <= length:
            raise _invalid("chunk out-of-range count is out of range")
        chunks.append(
            ChunkDigest(
                start_sample=start,
                end_sample=end,
                signal_hash=signal_hash,
                limited_samples=limited_samples,
                out_of_range_samples=out_of_range_samples,
            )
        )
        cursor = end
    if cursor != sample_count:
        raise _invalid("chunks do not cover the published signal")
    return tuple(chunks)


def _load_chunk_payloads(
    chunks_raw: list[dict[str, Any]],
    digests: tuple[ChunkDigest, ...],
    sample_count: int,
    config: ProjectConfig,
    segment_signals: tuple[SegmentSignal, ...],
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, tuple[tuple[int, int], ...]]:
    left = np.zeros(sample_count, dtype=np.float64)
    right = np.zeros(sample_count, dtype=np.float64)
    limited_mask = np.zeros(sample_count, dtype=np.bool_)
    out_of_range_mask = np.zeros(sample_count, dtype=np.bool_)
    recomputed: list[tuple[int, int]] = []

    for raw, digest in zip(chunks_raw, digests):
        start = digest.start_sample
        end = digest.end_sample
        count = end - start
        complete = raw.get("complete") is True
        chunk_left = (
            _decode_f64(raw.get("left_b64"), count, "left_b64") if complete else None
        )
        chunk_right = (
            _decode_f64(raw.get("right_b64"), count, "right_b64") if complete else None
        )
        chunk_limited = (
            _decode_mask(raw.get("limited_mask_b64"), count) if complete else None
        )
        chunk_out = (
            _decode_mask(raw.get("out_of_range_mask_b64"), count) if complete else None
        )
        valid = not (
            chunk_left is None
            or chunk_right is None
            or chunk_limited is None
            or chunk_out is None
        )
        if valid:
            signal_hash = chunk_signal_hash(
                start,
                end,
                chunk_left,
                chunk_right,
            )
            valid = (
                signal_hash == digest.signal_hash
                and int(chunk_limited.sum()) == digest.limited_samples
                and int(chunk_out.sum()) == digest.out_of_range_samples
            )
        if not valid:
            (
                chunk_left,
                chunk_right,
                chunk_limited,
                chunk_out,
            ) = _recompute_chunk(start, end, config, segment_signals)
            recomputed.append((start, end))
            signal_hash = chunk_signal_hash(
                start,
                end,
                chunk_left,
                chunk_right,
            )
            if signal_hash != digest.signal_hash:
                raise _invalid(
                    "rebuilt chunk hash does not match snapshot",
                    start_sample=start,
                    end_sample=end,
                )
            if (
                int(chunk_limited.sum()) != digest.limited_samples
                or int(chunk_out.sum()) != digest.out_of_range_samples
            ):
                raise _invalid(
                    "rebuilt chunk limiter counts do not match snapshot",
                    start_sample=start,
                    end_sample=end,
                )
        left[start:end] = chunk_left
        right[start:end] = chunk_right
        limited_mask[start:end] = chunk_limited
        out_of_range_mask[start:end] = chunk_out

    return (
        left,
        right,
        limited_mask,
        out_of_range_mask,
        tuple(recomputed),
    )
