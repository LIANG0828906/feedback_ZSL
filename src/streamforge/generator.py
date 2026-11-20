from __future__ import annotations

import hashlib
import json
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .errors import BusinessError, ErrorCode, fail
from .models import (
    GENERATOR_VERSION,
    SLICE_MAGIC,
    BudgetConfig,
    MemoryPlan,
    SliceArtifact,
    SliceBoundary,
    SourceTimeline,
    TierSpec,
    canonical_json,
    sha256_hex,
    sha256_json,
)


def tier_fingerprint(tier: TierSpec, timeline: SourceTimeline) -> str:
    return sha256_json(
        {
            "generator_version": GENERATOR_VERSION,
            "source_audio": timeline.audio.raw_digest,
            "tier": tier.to_dict(),
            "timeline_digest": timeline.digest,
            "video": timeline.video_digest,
        }
    )


def slice_output_samples(boundary: SliceBoundary, sample_rate: int) -> int:
    start = (boundary.start_ns * sample_rate + 500_000_000) // 1_000_000_000
    end = (boundary.end_ns * sample_rate + 500_000_000) // 1_000_000_000
    return max(
        0,
        end - start,
    )


def slice_frame_count(timeline: SourceTimeline, boundary: SliceBoundary) -> int:
    return len(timeline.frames_for_boundary(boundary))


def estimate_memory(
    *,
    tier: TierSpec,
    timeline: SourceTimeline,
    boundary: SliceBoundary,
    budget: BudgetConfig,
) -> MemoryPlan:
    sample_count = slice_output_samples(boundary, tier.audio_sample_rate)
    channels = timeline.audio.channels
    audio_bytes = sample_count * channels * 2
    bitrate_bytes = (
        tier.target_bitrate_kbps * 1000 * boundary.duration_ns + 4_000_000_000
    ) // 8_000_000_000
    frame_count = slice_frame_count(timeline, boundary)
    video_bytes = bitrate_bytes + frame_count * 192
    base = audio_bytes + video_bytes
    minimum_working = budget.min_block_bytes * channels * 8
    budget_left = budget.single_slice_memory_bytes - base
    if budget_left < minimum_working:
        raise fail(
            ErrorCode.BUDGET_EXCEEDED,
            "slice payload exceeds per-slice memory budget",
            tier=tier.tier_id,
            slice_index=boundary.index,
            audio_bytes=audio_bytes,
            video_bytes=video_bytes,
            required_bytes=base + minimum_working,
            single_slice_budget_bytes=budget.single_slice_memory_bytes,
        )
    block_bytes = min(
        budget.initial_block_bytes,
        max(1, budget_left // (channels * 8)),
    )
    working_bytes = block_bytes * channels * 8
    estimated_bytes = base + working_bytes
    return MemoryPlan(
        audio_bytes=audio_bytes,
        video_bytes=video_bytes,
        block_bytes=block_bytes,
        working_bytes=working_bytes,
        estimated_bytes=estimated_bytes,
        degraded=block_bytes < budget.initial_block_bytes,
    )


def _gain_fixed(gain_db: float) -> int:
    factor = 10.0 ** (gain_db / 20.0)
    return max(0, int(round(factor * 65536.0)))


def _resample_slice(
    timeline: SourceTimeline,
    tier: TierSpec,
    boundary: SliceBoundary,
    plan: MemoryPlan,
    cancel_event: threading.Event | None,
) -> np.ndarray:
    source = timeline.audio.samples
    source_rate = timeline.audio.sample_rate
    target_rate = tier.audio_sample_rate
    channels = timeline.audio.channels
    start_out = (boundary.start_ns * target_rate + 500_000_000) // 1_000_000_000
    end_out = (boundary.end_ns * target_rate + 500_000_000) // 1_000_000_000
    count = max(0, end_out - start_out)
    output = np.empty((count, channels), dtype=np.int16)
    gain = _gain_fixed(tier.gain_db)
    block = max(1, plan.block_bytes // max(1, channels * 4))
    source_count = source.shape[0]
    for block_start in range(0, count, block):
        if cancel_event is not None and cancel_event.is_set():
            raise BusinessError(ErrorCode.CANCELLED, "slice generation cancelled")
        block_end = min(count, block_start + block)
        positions = (start_out + np.arange(block_start, block_end, dtype=np.int64)) * source_rate
        indices = positions // target_rate
        remainders = positions % target_rate
        np.minimum(indices, source_count - 1, out=indices)
        next_indices = np.minimum(indices + 1, source_count - 1)
        first = source[indices].astype(np.int64)
        second = source[next_indices].astype(np.int64)
        interpolated = (
            first * (target_rate - remainders[:, None])
            + second * remainders[:, None]
            + target_rate // 2
        ) // target_rate
        adjusted = (interpolated * gain + 32768) // 65536
        np.clip(adjusted, -32768, 32767, out=adjusted)
        output[block_start:block_end] = adjusted.astype(np.int16)
    return output


def _deterministic_payload(seed: str, size: int) -> bytes:
    if size <= 0:
        return b""
    digest = hashlib.sha256(seed.encode("utf-8")).digest()
    repeats = (size + len(digest) - 1) // len(digest)
    return (digest * repeats)[:size]


def _video_payload(
    timeline: SourceTimeline,
    tier: TierSpec,
    boundary: SliceBoundary,
) -> tuple[bytes, list[dict[str, Any]], int]:
    frames = timeline.frames_for_boundary(boundary)
    video_parts: list[bytes] = []
    frame_headers: list[dict[str, Any]] = []
    offset = 0
    for frame in frames:
        media_bytes = max(
            32,
            (
                tier.target_bitrate_kbps
                * 1000
                * frame.duration_ns
                + 4_000_000_000
            )
            // 8_000_000_000,
        )
        seed = (
            f"{tier.tier_id}|{tier.video_qp}|{frame.display_index}|"
            f"{frame.pts_ns}|{frame.duration_ns}"
        )
        payload = _deterministic_payload(seed, media_bytes)
        digest = sha256_hex(payload)
        video_parts.append(payload)
        frame_headers.append(
            {
                "display_index": frame.display_index,
                "duration_ns": frame.duration_ns,
                "length": len(payload),
                "offset": offset,
                "payload_digest": digest,
                "pts_ns": frame.pts_ns,
            }
        )
        offset += len(payload)
    return b"".join(video_parts), frame_headers, offset


def build_slice_bytes(
    *,
    timeline: SourceTimeline,
    tier: TierSpec,
    boundary: SliceBoundary,
    prev_hash: str,
    plan: MemoryPlan,
    cancel_event: threading.Event | None = None,
) -> tuple[bytes, SliceArtifact]:
    audio = _resample_slice(timeline, tier, boundary, plan, cancel_event)
    audio_bytes = audio.astype("<i2", copy=False).tobytes(order="C")
    video_bytes, frame_headers, video_size = _video_payload(
        timeline, tier, boundary
    )
    payload = audio_bytes + video_bytes
    frames = timeline.frames_for_boundary(boundary)
    if not frames:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "slice contains no video frame",
            tier=tier.tier_id,
            slice_index=boundary.index,
        )
    output_start = (boundary.start_ns * tier.audio_sample_rate + 500_000_000) // 1_000_000_000
    output_end = (boundary.end_ns * tier.audio_sample_rate + 500_000_000) // 1_000_000_000
    header: dict[str, Any] = {
        "audio_bytes": len(audio_bytes),
        "audio_channels": timeline.audio.channels,
        "audio_end_ns": boundary.end_ns,
        "audio_sample_end": output_end,
        "audio_sample_rate": tier.audio_sample_rate,
        "audio_sample_start": output_start,
        "audio_start_ns": boundary.start_ns,
        "end_ns": boundary.end_ns,
        "frames": frame_headers,
        "generator_version": GENERATOR_VERSION,
        "index": boundary.index,
        "prev_hash": prev_hash,
        "start_ns": boundary.start_ns,
        "tier": tier.tier_id,
        "video_bytes": video_size,
        "video_end_ns": boundary.end_ns,
        "video_start_ns": frames[0].pts_ns,
    }
    header["content_hash"] = sha256_hex(
        canonical_json(header) + b"\x00" + payload
    )
    encoded_header = canonical_json(header)
    file_bytes = (
        SLICE_MAGIC
        + len(encoded_header).to_bytes(4, "big")
        + encoded_header
        + payload
    )
    artifact = SliceArtifact.from_header(
        header,
        path=Path(),
        file_size=len(file_bytes),
        memory=plan,
    )
    return file_bytes, artifact


def parse_slice_file(path: Path, *, verify: bool = True) -> SliceArtifact:
    try:
        data = path.read_bytes()
    except FileNotFoundError as exc:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "slice file is missing",
            path=str(path),
        ) from exc
    if len(data) < 12 or data[:8] != SLICE_MAGIC:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "invalid slice file header",
            path=str(path),
        )
    header_size = int.from_bytes(data[8:12], "big")
    if header_size <= 0 or len(data) < 12 + header_size:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "truncated slice header",
            path=str(path),
        )
    try:
        header = json.loads(data[12 : 12 + header_size].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "corrupt slice header",
            path=str(path),
        ) from exc
    payload = data[12 + header_size :]
    if verify:
        expected = header.get("content_hash", "")
        unsigned = dict(header)
        unsigned.pop("content_hash", None)
        actual = sha256_hex(canonical_json(unsigned) + b"\x00" + payload)
        if expected != actual:
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "slice content hash mismatch",
                path=str(path),
                expected=expected,
                actual=actual,
            )
        if len(payload) != int(header["audio_bytes"]) + int(header["video_bytes"]):
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "slice payload length mismatch",
                path=str(path),
            )
    return SliceArtifact.from_header(
        header,
        path=path,
        file_size=len(data),
        memory=MemoryPlan(
            audio_bytes=int(header["audio_bytes"]),
            video_bytes=int(header["video_bytes"]),
            block_bytes=0,
            working_bytes=0,
            estimated_bytes=int(header["audio_bytes"]) + int(header["video_bytes"]),
            degraded=False,
        ),
    )


def atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp")
    try:
        with temporary.open("wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            temporary.unlink(missing_ok=True)
        finally:
            raise


@dataclass
class SliceWriteResult:
    artifact: SliceArtifact


class SliceWriter:
    def __init__(self, tier_dir: Path) -> None:
        self.tier_dir = tier_dir
        self.tier_dir.mkdir(parents=True, exist_ok=True)

    def path_for(self, index: int) -> Path:
        return self.tier_dir / f"slice-{index:05d}.fslice"

    def cleanup_failed(self, index: int) -> None:
        path = self.path_for(index)
        path.unlink(missing_ok=True)
        path.with_name(f".{path.name}.tmp").unlink(missing_ok=True)

    def write(
        self,
        *,
        timeline: SourceTimeline,
        tier: TierSpec,
        boundary: SliceBoundary,
        prev_hash: str,
        plan: MemoryPlan,
        cancel_event: threading.Event | None = None,
        pre_commit: Callable[[str, int], None] | None = None,
    ) -> SliceArtifact:
        data, artifact = build_slice_bytes(
            timeline=timeline,
            tier=tier,
            boundary=boundary,
            prev_hash=prev_hash,
            plan=plan,
            cancel_event=cancel_event,
        )
        if pre_commit is not None:
            pre_commit(tier.tier_id, boundary.index)
        path = self.path_for(boundary.index)
        atomic_write(path, data)
        artifact.path = path
        return artifact


def parse_tier_manifest(path: Path) -> dict[str, Any] | None:
    if not path.exists():
        return None
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "corrupt tier manifest",
            path=str(path),
            error=str(exc),
        ) from exc
    if not isinstance(value, dict):
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "tier manifest must be an object",
            path=str(path),
        )
    return value
