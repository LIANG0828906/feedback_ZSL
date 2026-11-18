from __future__ import annotations

import json
import hashlib
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf


def _difference(
    block: int,
    channel: str,
    difference_type: str,
    *,
    first_sample: int | None = None,
    count: int = 0,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "block": int(block),
        "channel": channel,
        "difference_type": difference_type,
    }
    if first_sample is not None:
        payload["first_sample"] = int(first_sample)
    payload["count"] = int(count)
    return payload


def _compare_audio(
    reference_path: Path,
    candidate_path: Path,
    labels: tuple[str, ...],
    block_frames: int,
) -> list[dict[str, Any]]:
    differences: list[dict[str, Any]] = []
    if not reference_path.is_file() or not candidate_path.is_file():
        return [_difference(-1, "audio", "missing_file")]
    def digest(path: Path) -> str:
        value = hashlib.sha256()
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                value.update(chunk)
        return value.hexdigest()

    ref_digest = digest(reference_path)
    cand_digest = digest(candidate_path)
    if ref_digest == cand_digest:
        return differences
    ref_info = sf.info(str(reference_path))
    cand_info = sf.info(str(candidate_path))
    if (
        ref_info.samplerate != cand_info.samplerate
        or ref_info.channels != cand_info.channels
        or ref_info.subtype != cand_info.subtype
    ):
        differences.append(_difference(-1, "audio", "format_mismatch"))
    reference, _ = sf.read(str(reference_path), dtype="float32", always_2d=True)
    candidate, _ = sf.read(str(candidate_path), dtype="float32", always_2d=True)
    total = max(reference.shape[0], candidate.shape[0])
    channels = max(reference.shape[1], candidate.shape[1], len(labels))
    for channel in range(channels):
        label = labels[channel] if channel < len(labels) else f"channel_{channel}"
        for block, start in enumerate(range(0, total, block_frames)):
            end = min(total, start + block_frames)
            ref_valid = start < reference.shape[0] and channel < reference.shape[1]
            cand_valid = start < candidate.shape[0] and channel < candidate.shape[1]
            if not ref_valid or not cand_valid:
                differences.append(
                    _difference(block, label, "length_mismatch", first_sample=start, count=end - start)
                )
                continue
            ref_end = min(end, reference.shape[0])
            cand_end = min(end, candidate.shape[0])
            if ref_end - start != cand_end - start:
                differences.append(
                    _difference(
                        block,
                        label,
                        "length_mismatch",
                        first_sample=start,
                        count=max(ref_end, cand_end) - start,
                    )
                )
                continue
            ref_segment = np.ascontiguousarray(reference[start:ref_end, channel])
            cand_segment = np.ascontiguousarray(candidate[start:cand_end, channel])
            ref_bits = ref_segment.view(np.uint32)
            cand_bits = cand_segment.view(np.uint32)
            mismatch = ref_bits != cand_bits
            if np.any(mismatch):
                first = int(np.flatnonzero(mismatch)[0])
                differences.append(
                    _difference(
                        block,
                        label,
                        "byte_mismatch",
                        first_sample=start + first,
                        count=int(np.count_nonzero(mismatch)),
                    )
                )
    if not differences and ref_digest != cand_digest:
        differences.append(
            _difference(
                -1,
                "header",
                "byte_mismatch",
                first_sample=0,
                count=reference_path.stat().st_size,
            )
        )
    return differences


def verify_outputs(
    reference_dir: str | Path,
    candidate_dir: str | Path,
    *,
    block_frames: int = 48_000,
    stereo_name: str = "rainforest_stereo_master.wav",
    mono_name: str = "rainforest_mono_master.wav",
    report_name: str = "render_report.json",
) -> list[dict[str, Any]]:
    reference = Path(reference_dir)
    candidate = Path(candidate_dir)
    differences: list[dict[str, Any]] = []
    differences.extend(
        _compare_audio(reference / stereo_name, candidate / stereo_name, ("left", "right"), block_frames)
    )
    differences.extend(
        _compare_audio(reference / mono_name, candidate / mono_name, ("mono",), block_frames)
    )
    ref_report = reference / report_name
    cand_report = candidate / report_name
    if not ref_report.is_file() or not cand_report.is_file():
        differences.append(_difference(-1, "report", "missing_file"))
    elif ref_report.read_bytes() != cand_report.read_bytes():
        differences.append(_difference(-1, "report", "byte_mismatch"))
    return differences


def differences_exit_payload(differences: list[dict[str, Any]]) -> dict[str, Any]:
    return {
        "code": "E_RENDER_DIFFERENCE",
        "difference_count": len(differences),
        "differences": differences,
    }


def write_differences(path: str | Path, differences: list[dict[str, Any]]) -> None:
    Path(path).write_text(
        json.dumps(differences_exit_payload(differences), ensure_ascii=False, indent=2, sort_keys=True)
        + "\n",
        encoding="utf-8",
    )

