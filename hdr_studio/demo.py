from __future__ import annotations

import shutil
from pathlib import Path
from typing import Any

from .errors import HDRError
from .generator import DEFAULT_SAMPLES, generate_sample_set
from .model import (
    ColorSpace,
    ConversionParams,
    ExposureSegment,
    ToneMap,
    TransferFunction,
)
from .session import ConversionSession
from .util import atomic_write_json, canonical_bytes, hash_file, sha256_hex

PRIMARY_SAMPLE = DEFAULT_SAMPLES[0].name


def _reset_generated_state(workspace: Path) -> None:
    for name in ("samples", "versions", "snapshots", "trace"):
        shutil.rmtree(workspace / name, ignore_errors=True)
    for name in ("baselines.json", "demo-result.json"):
        (workspace / name).unlink(missing_ok=True)


def _version_digest(record: Any) -> dict[str, Any]:
    return {
        "version": record.version,
        "output_hash": record.output_hash,
        "record_hash": record.record_hash,
        "frame_hashes": list(record.frame_hashes),
        "params": record.params.to_dict(),
        "affected_frames": record.affected_count,
        "recomputed_frames": record.recomputed_count,
        "reused_frames": record.reused_count,
    }


def run_demo(
    workspace: Path | str,
    *,
    seed: int = 20261121,
    force: bool = False,
) -> dict[str, Any]:
    workspace = Path(workspace)
    workspace.mkdir(parents=True, exist_ok=True)
    if force:
        _reset_generated_state(workspace)
    elif any(
        (workspace / name).exists()
        for name in ("baselines.json", "snapshots/current.json", "trace/trace.jsonl")
    ):
        raise FileExistsError(
            f"{workspace} already contains demo state; pass force=True to rebuild it"
        )

    manifest = generate_sample_set(workspace, seed=seed)
    primary = workspace / "samples" / f"{PRIMARY_SAMPLE}.hdrseq"
    session = ConversionSession(workspace, primary)
    summaries: list[dict[str, Any]] = []
    rejections: list[dict[str, Any]] = []

    v1_params = ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.REINHARD,
    )
    v1 = session.submit(v1_params)
    summaries.append(_version_digest(v1))

    v2_params = ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.REINHARD,
        exposure_segments=(
            ExposureSegment(2, 4, -0.5),
            ExposureSegment(5, 6, -0.25),
        ),
    )
    v2 = session.submit(v2_params)
    summaries.append(_version_digest(v2))

    invalid_peak = ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.NONE,
    )
    try:
        session.submit(invalid_peak)
    except HDRError as exc:
        rejections.append(exc.to_dict())
        if exc.code.value != "E_PARAM_CONFLICT":
            raise

    invalid_exposure = ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.REINHARD,
        exposure_segments=(
            ExposureSegment(2, 4, -0.5),
            ExposureSegment(5, 6, 4.0),
        ),
    )
    try:
        session.submit(invalid_exposure)
    except HDRError as exc:
        rejections.append(exc.to_dict())
        if exc.code.value != "E_PARAM_CONFLICT":
            raise

    v3_params = ConversionParams(
        target_color_space=ColorSpace.DISPLAY_P3,
        target_transfer=TransferFunction.HLG,
        peak_luminance=600.0,
        tone_map=ToneMap.HABLE,
        exposure_segments=(
            ExposureSegment(2, 4, -0.5),
            ExposureSegment(5, 6, -0.25),
        ),
    )
    v3 = session.submit(v3_params)
    summaries.append(_version_digest(v3))

    v4 = session.rollback(v2.version)
    summaries.append(_version_digest(v4))
    preview = session.set_preview(
        selected_version=v3.version, frame_index=2, playing=False
    )
    latest_before_recovery = session.latest_record
    assert latest_before_recovery is not None
    context = {
        "seed": seed,
        "sample": PRIMARY_SAMPLE,
        "sample_set_hash": manifest["sample_set_hash"],
        "versions": summaries,
        "rejections": rejections,
        "preview_before_recovery": preview.to_dict(),
        "latest_version_before_recovery": latest_before_recovery.version,
        "latest_output_hash_before_recovery": latest_before_recovery.output_hash,
    }
    session.close()

    recovered = ConversionSession(workspace)
    latest_after_recovery = recovered.latest_record
    assert latest_after_recovery is not None
    if latest_after_recovery.output_hash != latest_before_recovery.output_hash:
        raise RuntimeError("recovery changed the last published output hash")
    if recovered.preview_state != preview:
        raise RuntimeError("recovery did not restore the preview state")
    recovery_report = recovered.recovery_report.to_dict() if recovered.recovery_report else None
    recovered.close()

    trace_path = workspace / "trace" / "trace.jsonl"
    baseline = {
        "schema_version": 1,
        "seed": seed,
        "sample": PRIMARY_SAMPLE,
        "sample_set_hash": manifest["sample_set_hash"],
        "versions": summaries,
        "preview": preview.to_dict(),
        "latest_version_after_recovery": latest_after_recovery.version,
        "latest_output_hash_after_recovery": latest_after_recovery.output_hash,
        "truncated": False,
        "rejection_count": len(rejections),
        "trace_hash": hash_file(trace_path),
        "recovery": recovery_report,
        "context_hash": sha256_hex(canonical_bytes(context)),
    }
    atomic_write_json(workspace / "baselines.json", baseline)
    return baseline

