from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, WorkbenchError, meta_error, para_error
from .layout import ChannelLayout, parse_layout
from .model import (
    AcousticSample,
    ChannelTruthSummary,
    FrameTruth,
    parse_construction_channel,
)


def _read_json(path: Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise meta_error(f"cannot read manifest: {exc}", "manifest") from exc
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise meta_error("manifest is not valid UTF-8 JSON", "manifest") from exc
    if not isinstance(payload, dict):
        raise meta_error("manifest root must be an object", "manifest")
    return payload, raw


def load_manifest(path: str | Path) -> dict[str, Any]:
    payload, _ = _read_json(Path(path))
    samples = payload.get("samples")
    if not isinstance(samples, list) or not samples:
        raise meta_error("manifest contains no samples", "samples")
    return payload


def _sample_payload(payload: dict[str, Any], sample_id: str | None) -> dict[str, Any]:
    samples = payload.get("samples")
    if not isinstance(samples, list):
        raise meta_error("manifest contains no samples array", "samples")
    if sample_id is None:
        if len(samples) != 1:
            raise meta_error(
                "sample_id is required for a multi-sample manifest",
                "sample_id",
            )
        selected = samples[0]
    else:
        selected = None
        for item in samples:
            if isinstance(item, dict) and item.get("sample_id") == sample_id:
                selected = item
                break
        if selected is None:
            raise meta_error(f"sample {sample_id!r} is not in the manifest", "sample_id")
    if not isinstance(selected, dict):
        raise meta_error("sample entry must be an object", "samples")
    return selected


def _load_array(path: Path, field: str) -> np.ndarray:
    try:
        if path.suffix.lower() == ".npz":
            with np.load(path, allow_pickle=False) as archive:
                if "ir" in archive.files:
                    array = archive["ir"]
                elif "data" in archive.files:
                    array = archive["data"]
                elif len(archive.files) == 1:
                    array = archive[archive.files[0]]
                else:
                    raise meta_error(
                        "npz array has no unambiguous data key",
                        field,
                    )
        elif path.suffix.lower() == ".npy":
            array = np.load(path, allow_pickle=False)
        else:
            raise meta_error("array file must use .npy or .npz", field)
    except WorkbenchError:
        raise
    except (OSError, ValueError) as exc:
        raise meta_error(f"cannot load array file: {exc}", field) from exc
    return np.asarray(array, dtype=np.float64)


def _parse_declared_truth(
    sample_payload: dict[str, Any],
    layout: ChannelLayout,
) -> tuple[ChannelTruthSummary, ...]:
    truth_payload = sample_payload.get("truth")
    if not isinstance(truth_payload, dict):
        raise meta_error("per-channel analytic truth is missing", "truth")
    channels = truth_payload.get("channels")
    if not isinstance(channels, list):
        raise meta_error("truth.channels must be an array", "truth.channels")
    if len(channels) != layout.count:
        raise WorkbenchError(
            ErrorCode.CHAN,
            "truth channel count does not match declared layout",
            field="truth.channels",
            details={"truth_channels": len(channels), "layout_channels": layout.count},
        )

    rows: list[ChannelTruthSummary] = []
    required = (
        "direct_delay_samples",
        "direct_delay_s",
        "early_reflection_energy",
        "reverb_energy",
        "drr",
        "tdoa_samples",
        "tdoa_s",
    )
    for channel_index, item in enumerate(channels):
        if not isinstance(item, dict):
            raise meta_error(
                "truth channel entry must be an object",
                f"truth.channels[{channel_index}]",
            )
        channel_id = item.get("channel")
        if channel_id != layout.channels[channel_index].id:
            raise WorkbenchError(
                ErrorCode.CHAN,
                "truth channel order does not match declared layout",
                field=f"truth.channels[{channel_index}].channel",
                channel=channel_id,
                details={"expected": layout.channels[channel_index].id},
            )
        values: dict[str, float] = {}
        for key in required:
            if key not in item:
                raise meta_error(
                    "required analytic truth field is missing",
                    f"truth.channels[{channel_index}].{key}",
                )
            try:
                value = float(item[key])
            except (TypeError, ValueError) as exc:
                raise meta_error(
                    "analytic truth value must be numeric",
                    f"truth.channels[{channel_index}].{key}",
                ) from exc
            if not math.isfinite(value):
                raise meta_error(
                    "analytic truth value must be finite",
                    f"truth.channels[{channel_index}].{key}",
                )
            values[key] = value

        frame_payload = item.get("frames", [])
        if not isinstance(frame_payload, list):
            raise meta_error(
                "truth frames must be an array",
                f"truth.channels[{channel_index}].frames",
            )
        frames: list[FrameTruth] = []
        for frame_item in frame_payload:
            if not isinstance(frame_item, dict):
                raise meta_error(
                    "truth frame entry must be an object",
                    f"truth.channels[{channel_index}].frames",
                )
            try:
                frame_index = int(frame_item["frame_index"])
                reflections = tuple(
                    int(value)
                    for value in frame_item.get("reflection_delays_samples", [])
                )
                frame = FrameTruth(
                    frame_index=frame_index,
                    channel_id=channel_id,
                    direct_delay_samples=float(frame_item["direct_delay_samples"]),
                    direct_delay_s=float(frame_item["direct_delay_s"]),
                    direct_energy=float(frame_item["direct_energy"]),
                    early_reflection_energy=float(
                        frame_item["early_reflection_energy"]
                    ),
                    reverb_energy=float(frame_item["reverb_energy"]),
                    drr=float(frame_item["drr"]),
                    tdoa_samples=float(frame_item["tdoa_samples"]),
                    tdoa_s=float(frame_item["tdoa_s"]),
                    reflection_delays_samples=reflections,
                )
            except (KeyError, TypeError, ValueError) as exc:
                raise meta_error(
                    "truth frame has invalid fields",
                    f"truth.channels[{channel_index}].frames",
                ) from exc
            frames.append(frame)
        rows.append(
            ChannelTruthSummary(
                channel_id=channel_id,
                direct_delay_samples=values["direct_delay_samples"],
                direct_delay_s=values["direct_delay_s"],
                early_reflection_energy=values["early_reflection_energy"],
                reverb_energy=values["reverb_energy"],
                drr=values["drr"],
                tdoa_samples=values["tdoa_samples"],
                tdoa_s=values["tdoa_s"],
                frames=tuple(frames),
            )
        )
    return tuple(rows)


def load_sample(
    manifest_path: str | Path,
    sample_id: str | None = None,
) -> AcousticSample:
    manifest_path = Path(manifest_path)
    payload, raw = _read_json(manifest_path)
    sample_payload = _sample_payload(payload, sample_id)

    selected_id = sample_payload.get("sample_id")
    if not isinstance(selected_id, str) or not selected_id:
        raise meta_error("sample_id is missing", "sample_id")
    sample_rate_value = sample_payload.get("sample_rate")
    if sample_rate_value is None:
        raise meta_error("sample_rate is missing", "sample_rate")
    try:
        sample_rate = int(sample_rate_value)
    except (TypeError, ValueError) as exc:
        raise meta_error("sample_rate must be an integer", "sample_rate") from exc
    if sample_rate <= 0:
        raise meta_error("sample_rate must be positive", "sample_rate")

    if sample_payload.get("channel_layout") is None:
        raise meta_error("channel_layout is missing", "channel_layout")
    layout = parse_layout(sample_payload.get("channel_layout"))

    base_dir = manifest_path.parent
    ir_name = sample_payload.get("ir_file")
    dry_name = sample_payload.get("dry_file")
    if not isinstance(ir_name, str) or not ir_name:
        raise meta_error("ir_file is missing", "ir_file")
    if not isinstance(dry_name, str) or not dry_name:
        raise meta_error("dry_file is missing", "dry_file")
    ir_path = base_dir / ir_name
    dry_path = base_dir / dry_name
    ir = _load_array(ir_path, "ir")
    dry = _load_array(dry_path, "dry")
    if ir.ndim == 1:
        ir = ir[np.newaxis, :]
    if dry.ndim == 1:
        dry = dry[np.newaxis, :]
    if ir.ndim != 2:
        raise para_error("IR must have shape [channels, samples]", "ir")
    if dry.ndim != 2:
        raise para_error("dry clip must have shape [channels, samples]", "dry")
    if ir.shape[0] != layout.count:
        raise WorkbenchError(
            ErrorCode.CHAN,
            "IR channel count does not match declared layout",
            field="ir",
            details={"ir_channels": int(ir.shape[0]), "layout_channels": layout.count},
        )
    if ir.shape[1] == 0 or dry.shape[1] == 0:
        raise para_error("IR and dry clip must contain samples", "ir/dry")

    declared_truth = _parse_declared_truth(sample_payload, layout)
    construction_payload = sample_payload.get("construction")
    if not isinstance(construction_payload, dict):
        raise meta_error("analytic construction metadata is missing", "construction")
    construction = []
    for channel_spec in layout.channels:
        channel_payload = construction_payload.get(channel_spec.id)
        if not isinstance(channel_payload, dict):
            raise meta_error(
                "channel construction metadata is missing",
                f"construction.{channel_spec.id}",
            )
        construction.append(
            parse_construction_channel(
                channel_payload,
                channel_spec.id,
                sample_rate,
            )
        )

    manifest_sha256 = hashlib.sha256(raw).hexdigest()
    return AcousticSample(
        sample_id=selected_id,
        sample_rate=sample_rate,
        layout=layout,
        ir=ir,
        dry=dry,
        construction=tuple(construction),
        declared_truth=declared_truth,
        manifest_sha256=manifest_sha256,
        manifest_path=str(manifest_path),
        ir_file=str(ir_path),
        dry_file=str(dry_path),
    )
