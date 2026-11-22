"""Sample loading with explicit metadata, decode and channel failures."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, WorkbenchError
from .models import AcousticSample, ChannelLayout


def _parse_metadata(raw: Any) -> dict[str, Any]:
    if raw is None:
        raise WorkbenchError(
            ErrorCode.E_META,
            "sample metadata is missing",
            stage="load",
        )
    try:
        if isinstance(raw, np.ndarray):
            raw = raw.item()
        if isinstance(raw, bytes):
            raw = raw.decode("utf-8")
        metadata = json.loads(str(raw))
    except (UnicodeDecodeError, ValueError, TypeError) as exc:
        raise WorkbenchError(
            ErrorCode.E_META,
            "sample metadata is not valid JSON",
            stage="load",
        ) from exc
    if not isinstance(metadata, dict):
        raise WorkbenchError(
            ErrorCode.E_META,
            "sample metadata must be a JSON object",
            stage="load",
        )
    required = ("sample_rate", "channel_layout", "truth")
    missing = [key for key in required if key not in metadata]
    if missing:
        raise WorkbenchError(
            ErrorCode.E_META,
            "required sample metadata is missing",
            stage="load",
            details={"missing": missing},
        )
    return metadata


def _decode_array(name: str, value: np.ndarray) -> np.ndarray:
    array = np.asarray(value)
    if array.dtype.kind not in "fiu":
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            f"{name} array has unsupported dtype {array.dtype}",
            stage="decode",
        )
    result = np.asarray(array, dtype=np.float64)
    if not np.all(np.isfinite(result)):
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            f"{name} contains NaN or infinite samples",
            stage="decode",
        )
    return result


def load_sample(path: str | Path) -> AcousticSample:
    source = Path(path)
    if not source.is_file():
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            f"sample file does not exist: {source}",
            stage="load",
        )
    try:
        archive = np.load(source, allow_pickle=False)
    except Exception as exc:
        raise WorkbenchError.wrap(
            ErrorCode.E_DECODE,
            f"sample cannot be decoded: {source}",
            exc,
            stage="load",
            details={"path": str(source)},
        )
    try:
        with archive:
            if "metadata" not in archive.files:
                raise WorkbenchError(
                    ErrorCode.E_META,
                    "sample metadata field is missing",
                    stage="load",
                    details={"path": str(source)},
                )
            metadata = _parse_metadata(archive["metadata"])
            if "ir" not in archive.files or "dry" not in archive.files:
                raise WorkbenchError(
                    ErrorCode.E_DECODE,
                    "sample is missing the ir or dry array",
                    stage="decode",
                    details={"fields": list(archive.files)},
                )
            ir = _decode_array("ir", archive["ir"])
            dry = _decode_array("dry", archive["dry"])
    except WorkbenchError:
        raise
    except Exception as exc:
        raise WorkbenchError.wrap(
            ErrorCode.E_DECODE,
            f"sample cannot be decoded: {source}",
            exc,
            stage="decode",
        )

    try:
        sample_rate = int(metadata["sample_rate"])
    except (TypeError, ValueError) as exc:
        raise WorkbenchError(
            ErrorCode.E_META,
            "sample_rate metadata must be an integer",
            stage="load",
        ) from exc
    if sample_rate <= 0:
        raise WorkbenchError(
            ErrorCode.E_META,
            "sample_rate metadata must be positive",
            stage="load",
        )
    layout_value = metadata.get("channel_layout")
    if not isinstance(layout_value, dict):
        raise WorkbenchError(
            ErrorCode.E_META,
            "channel_layout metadata is missing or invalid",
            stage="load",
        )
    layout = ChannelLayout.from_metadata(layout_value)

    if ir.ndim != 2:
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            "ir must be a two-dimensional channel-by-sample array",
            stage="decode",
            details={"shape": list(ir.shape)},
        )
    if dry.ndim == 2 and dry.shape[0] == 1:
        dry = dry[0]
    if dry.ndim != 1:
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            "dry must be a mono one-dimensional array",
            stage="decode",
            details={"shape": list(dry.shape)},
        )
    if ir.shape[0] == 0 or ir.shape[1] == 0 or dry.size == 0:
        raise WorkbenchError(
            ErrorCode.E_DECODE,
            "ir and dry must be non-empty",
            stage="decode",
        )

    missing_channels = tuple(str(item) for item in metadata.get("missing_channels", []))
    if missing_channels:
        unknown = [item for item in missing_channels if item not in layout.channels]
        if unknown:
            raise WorkbenchError(
                ErrorCode.E_CHAN,
                "missing_channels references unknown layout channels",
                stage="load",
                details={"missing_channels": list(unknown)},
            )
        raise WorkbenchError(
            ErrorCode.E_CHAN,
            "input sample is missing one or more channels",
            stage="load",
            details={"missing_channels": list(missing_channels)},
        )
    if ir.shape[0] != len(layout.channels):
        raise WorkbenchError(
            ErrorCode.E_CHAN,
            "ir channel count does not match channel_layout metadata",
            stage="load",
            details={
                "ir_channels": int(ir.shape[0]),
                "layout_channels": list(layout.channels),
            },
        )

    bad_frames: list[int] = []
    raw_bad_frames = metadata.get("bad_frames", [])
    if raw_bad_frames is not None:
        if not isinstance(raw_bad_frames, (list, tuple)):
            raise WorkbenchError(
                ErrorCode.E_FRAME,
                "bad_frames metadata must be an array",
                stage="load",
            )
        for value in raw_bad_frames:
            try:
                frame_index = int(value)
            except (TypeError, ValueError) as exc:
                raise WorkbenchError(
                    ErrorCode.E_FRAME,
                    "bad_frames contains a non-integer frame index",
                    stage="load",
                ) from exc
            if frame_index < 0:
                raise WorkbenchError(
                    ErrorCode.E_FRAME,
                    "bad_frames contains a negative frame index",
                    stage="load",
                )
            bad_frames.append(frame_index)

    truth = metadata.get("truth")
    if not isinstance(truth, dict):
        raise WorkbenchError(
            ErrorCode.E_META,
            "analytic truth metadata is missing or invalid",
            stage="load",
        )
    raw_delays = truth.get("direct_delay_samples", [])
    raw_decay = truth.get("energy_decay_coeff", [])
    if len(raw_delays) != len(layout.channels):
        raise WorkbenchError(
            ErrorCode.E_META,
            "analytic direct delay truth does not match channel layout",
            stage="load",
        )
    if len(raw_decay) != len(layout.channels):
        raise WorkbenchError(
            ErrorCode.E_META,
            "analytic decay truth does not match channel layout",
            stage="load",
        )
    try:
        direct_truth = [int(value) for value in raw_delays]
        decay_truth = [float(value) for value in raw_decay]
    except (TypeError, ValueError) as exc:
        raise WorkbenchError(
            ErrorCode.E_META,
            "analytic truth values must be numeric",
            stage="load",
        ) from exc
    if any(value < 0 for value in direct_truth):
        raise WorkbenchError(
            ErrorCode.E_META,
            "analytic direct delays must be non-negative",
            stage="load",
        )
    if any(not np.isfinite(value) or value < 0.0 for value in decay_truth):
        raise WorkbenchError(
            ErrorCode.E_META,
            "analytic decay coefficients must be finite and non-negative",
            stage="load",
        )
    normalized_truth = dict(truth)
    normalized_truth["direct_delay_samples"] = direct_truth
    normalized_truth["energy_decay_coeff"] = decay_truth

    return AcousticSample(
        ir=np.ascontiguousarray(ir, dtype=np.float64),
        dry=np.ascontiguousarray(dry, dtype=np.float64),
        sample_rate=sample_rate,
        layout=layout,
        truth=normalized_truth,
        sample_id=str(metadata.get("sample_id", source.stem)),
        bad_frames=tuple(sorted(set(bad_frames))),
        missing_channels=(),
        source_path=str(source.resolve()),
    )

