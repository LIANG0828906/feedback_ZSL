from __future__ import annotations

import copy
import hashlib
from typing import Any, Iterable

import numpy as np

from .errors import drift_error
from .models import (
    ResampleRatio,
    TierParams,
    WavInfo,
    canonical_json_bytes,
    sha256_bytes,
)
from .resample import RenderedSlice, pcm_to_integers, render_slice, slice_chain_hash

SIDECAR_SCHEMA = "live-transcode-sidecar/v1"

_META_COMPARE_FIELDS = (
    "index",
    "out_start",
    "out_end",
    "src_start",
    "src_end",
    "sample_count",
    "phase_num",
    "phase_den",
    "phase_remainder_num",
    "end_phase_num",
    "peak_abs",
    "silent",
)


def make_slice_meta(rendered: RenderedSlice, previous_chain: str) -> dict[str, Any]:
    meta = rendered.meta_core()
    meta["prev_chain_hash"] = previous_chain
    meta["chain_hash"] = slice_chain_hash(previous_chain, rendered)
    return meta


def estimation_contract(info: WavInfo, tier: TierParams, ratio: ResampleRatio) -> dict[str, Any]:
    return {
        "observation_window": (
            "Each slice covers target samples [out_start, out_end). The sinc support is "
            "center-half+1 through center+half and is clipped only at the whole-file "
            "boundary; no support is reset at slice boundaries."
        ),
        "sync_markers": [
            "source_data_hash",
            "out_start",
            "out_end",
            "src_start",
            "src_end",
            "phase_num/phase_den",
            "end_phase_num",
        ],
        "truth_source": (
            "The SHA-256 of each committed PCM byte buffer is the only content truth. "
            "Peak and silence values are derived from decoded committed samples."
        ),
        "phase_definition": (
            "source position = output_index * step_num / step_den; phase_remainder_num = "
            "(phase_num + (filter_length//2)*step_num) % step_den. The next slice phase is "
            "the previous end_phase_num, so filter length participates in handoff."
        ),
        "step_num": ratio.step_num,
        "step_den": ratio.step_den,
        "source_rate": info.sample_rate,
        "target_rate": tier.target_rate,
        "filter_length": tier.filter_length,
    }


def build_sidecar(
    info: WavInfo,
    tier: TierParams,
    slice_metas: Iterable[dict[str, Any]],
    total_output_samples: int,
) -> dict[str, Any]:
    ratio = ResampleRatio.build(info.sample_rate, tier.target_rate)
    metas = list(slice_metas)
    sidecar: dict[str, Any] = {
        "schema": SIDECAR_SCHEMA,
        "material_id": info.material_id,
        "tier_id": tier.tier_id,
        "tier_params": tier.as_dict(),
        "param_version": tier.param_version,
        "source": info.as_dict(),
        "slice_samples": tier.slice_samples,
        "total_output_samples": total_output_samples,
        "total_slices": len(metas),
        "estimation": estimation_contract(info, tier, ratio),
        "slices": metas,
    }
    sidecar["tier_version_hash"] = compute_tier_version_hash(sidecar)
    return sidecar


def sidecar_bytes(sidecar: dict[str, Any]) -> bytes:
    return canonical_json_bytes(sidecar) + b"\n"


def compute_tier_version_hash(sidecar: dict[str, Any]) -> str:
    clone = copy.deepcopy(sidecar)
    clone.pop("tier_version_hash", None)
    return hashlib.sha256(canonical_json_bytes(clone)).hexdigest()


def _decode_meta_peak(
    pcm: bytes, channels: int, bits: int
) -> tuple[int, list[int], bool]:
    integers = pcm_to_integers(pcm, channels, bits)
    per_channel = np.max(np.abs(integers), axis=0).astype(np.int64)
    peak = int(per_channel.max(initial=0))
    return peak, [int(v) for v in per_channel], bool(peak == 0)


def _compare_meta(
    meta: dict[str, Any],
    expected: dict[str, Any],
) -> list[str]:
    differences: list[str] = []
    for field in _META_COMPARE_FIELDS:
        actual_value = meta.get(field)
        expected_value = expected[field]
        if actual_value != expected_value:
            differences.append(field)
    if list(meta.get("per_channel_peaks", [])) != list(expected["per_channel_peaks"]):
        differences.append("per_channel_peaks")
    return differences


def _classify_stale_side(
    *,
    audio_matches_expected: bool,
    sidecar_hash_matches_audio: bool,
    sidecar_hash_matches_expected: bool,
    meta_matches_expected: bool,
) -> str:
    if audio_matches_expected and meta_matches_expected:
        return "none"
    if audio_matches_expected:
        return "sidecar"
    if sidecar_hash_matches_expected:
        return "buffer"
    if sidecar_hash_matches_audio and not sidecar_hash_matches_expected:
        return "buffer"
    if not sidecar_hash_matches_audio and not sidecar_hash_matches_expected:
        return "both"
    return "both"


def audit_tier(
    source: np.ndarray,
    info: WavInfo,
    tier: TierParams,
    sidecar: dict[str, Any],
    audio_buffers: list[bytes],
    *,
    expected_version_hash: str | None = None,
) -> dict[str, Any]:
    if sidecar.get("schema") != SIDECAR_SCHEMA:
        raise drift_error(
            "sidecar schema is not recognized",
            0,
            "sidecar",
            schema=sidecar.get("schema"),
        )
    if sidecar.get("tier_params") != tier.as_dict():
        raise drift_error(
            "sidecar tier parameters do not match the requested immutable version",
            0,
            "sidecar",
            declared=sidecar.get("tier_params"),
            expected=tier.as_dict(),
        )
    ratio = ResampleRatio.build(info.sample_rate, tier.target_rate)
    total = ratio.output_length(info.frames)
    expected_total_slices = (total + tier.slice_samples - 1) // tier.slice_samples
    if sidecar.get("total_output_samples") != total:
        raise drift_error(
            "sidecar total output length disagrees with source and parameters",
            0,
            "sidecar",
            declared=sidecar.get("total_output_samples"),
            expected=total,
        )
    if sidecar.get("total_slices") != len(audio_buffers):
        raise drift_error(
            "sidecar slice count and audio buffer count disagree",
            min(sidecar.get("total_slices", 0), len(audio_buffers)),
            "sidecar" if sidecar.get("total_slices", 0) > len(audio_buffers) else "buffer",
            sidecar_slices=sidecar.get("total_slices"),
            audio_slices=len(audio_buffers),
        )

    version_hash = sidecar.get("tier_version_hash")
    computed_version_hash = compute_tier_version_hash(sidecar)
    drifting: list[dict[str, Any]] = []
    previous_chain = ""
    metas = sidecar.get("slices", [])
    for index in range(max(len(metas), len(audio_buffers))):
        if index >= len(metas) or index >= len(audio_buffers):
            stale = "sidecar" if index >= len(audio_buffers) else "buffer"
            drifting.append(
                {
                    "index": index,
                    "stale_side": stale,
                    "deviation_samples": 0,
                    "deviation_bytes": 0,
                    "details": ["missing slice on one side"],
                }
            )
            continue
        meta = metas[index]
        actual_pcm = audio_buffers[index]
        expected = render_slice(source, info, tier, index, total)
        expected_meta = expected.meta_core()
        actual_hash = sha256_bytes(actual_pcm)
        try:
            actual_peak, actual_channel_peaks, actual_silent = _decode_meta_peak(
                actual_pcm, info.channels, tier.quant_bits
            )
        except (ValueError, TypeError):
            actual_peak, actual_channel_peaks, actual_silent = None, None, None
        expected_peak = expected.peak_abs
        expected_channel_peaks = list(expected.per_channel_peaks)
        expected_silent = expected.silent
        sidecar_hash_matches_audio = meta.get("content_hash") == actual_hash
        sidecar_hash_matches_expected = meta.get("content_hash") == expected.content_hash
        audio_matches_expected = actual_pcm == expected.pcm_bytes
        meta_differences = _compare_meta(meta, expected_meta)
        if actual_peak != expected_peak:
            meta_differences.append("peak_abs_from_audio")
        if actual_channel_peaks != expected_channel_peaks:
            meta_differences.append("per_channel_peaks_from_audio")
        if actual_silent != expected_silent:
            meta_differences.append("silent_from_audio")
        if meta.get("content_hash") != expected.content_hash:
            meta_differences.append("content_hash")
        meta_matches_expected = not meta_differences

        chain_expected = slice_chain_hash(previous_chain, expected)
        if meta.get("prev_chain_hash") != previous_chain:
            meta_differences.append("prev_chain_hash")
        if meta.get("chain_hash") != chain_expected:
            meta_differences.append("chain_hash")
        meta_matches_expected = not meta_differences
        previous_chain = meta.get("chain_hash", "")

        deviation_bytes = 0
        deviation_samples = 0
        if not audio_matches_expected:
            deviation_bytes = abs(len(actual_pcm) - len(expected.pcm_bytes)) + sum(
                a != b for a, b in zip(actual_pcm, expected.pcm_bytes)
            )
            expected_ints = pcm_to_integers(expected.pcm_bytes, info.channels, tier.quant_bits)
            try:
                actual_ints = pcm_to_integers(
                    actual_pcm, info.channels, tier.quant_bits
                )
            except ValueError:
                actual_ints = None
            if actual_ints is None or actual_ints.shape != expected_ints.shape:
                deviation_samples = (
                    expected_ints.size
                    if actual_ints is None
                    else max(actual_ints.size, expected_ints.size)
                )
            else:
                deviation_samples = int(np.count_nonzero(actual_ints != expected_ints))
        interval_deviation = max(
            abs(int(meta.get("out_start", -1)) - expected.out_start),
            abs(int(meta.get("out_end", -1)) - expected.out_end),
            abs(int(meta.get("src_start", -1)) - expected.src_start),
            abs(int(meta.get("src_end", -1)) - expected.src_end),
        )

        if (
            audio_matches_expected
            and meta_matches_expected
            and sidecar_hash_matches_audio
        ):
            continue

        stale_side = _classify_stale_side(
            audio_matches_expected=audio_matches_expected,
            sidecar_hash_matches_audio=sidecar_hash_matches_audio,
            sidecar_hash_matches_expected=sidecar_hash_matches_expected,
            meta_matches_expected=meta_matches_expected,
        )
        drifting.append(
            {
                "index": index,
                "stale_side": stale_side,
                "deviation_samples": deviation_samples,
                "deviation_bytes": deviation_bytes,
                "interval_deviation_samples": interval_deviation,
                "meta_fields": meta_differences,
                "declared": {
                    "content_hash": meta.get("content_hash"),
                    "out_start": meta.get("out_start"),
                    "out_end": meta.get("out_end"),
                    "src_start": meta.get("src_start"),
                    "src_end": meta.get("src_end"),
                    "phase_remainder_num": meta.get("phase_remainder_num"),
                },
                "expected": {
                    "content_hash": expected.content_hash,
                    "out_start": expected.out_start,
                    "out_end": expected.out_end,
                    "src_start": expected.src_start,
                    "src_end": expected.src_end,
                    "phase_remainder_num": expected.phase_remainder_num,
                },
            }
        )

    report: dict[str, Any] = {
        "ok": not drifting,
        "tier_id": tier.tier_id,
        "param_version": tier.param_version,
        "tier_version_hash": version_hash,
        "computed_tier_version_hash": computed_version_hash,
        "expected_tier_version_hash": expected_version_hash,
        "expected_total_slices": expected_total_slices,
        "drifting_slices": drifting,
        "first_drift_slice": drifting[0]["index"] if drifting else None,
        "stale_side": drifting[0]["stale_side"] if drifting else "none",
        "deviation_slice_count": len(drifting),
        "total_deviation_samples": sum(item["deviation_samples"] for item in drifting),
    }
    if version_hash != computed_version_hash:
        report["version_hash_mismatch"] = True
    if drifting:
        raise drift_error(
            "sidecar and committed PCM buffers drift from regenerated truth",
            drifting[0]["index"],
            drifting[0]["stale_side"],
            report=report,
        )
    return report

