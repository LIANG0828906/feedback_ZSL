from __future__ import annotations

import json
import os
import random
import tempfile
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .canonical import bytes_hash
from .errors import ErrorCode, HapticError
from .models import DeviceConfig, Envelope, WaveSequence
from .sampler import compute_full_signal

DEFAULT_SEED = 20261122


@dataclass(frozen=True, slots=True)
class SampleSpec:
    sample_id: str
    segment_count: int
    overlap: str
    total_duration_ms: int
    seed_offset: int


SAMPLE_SPECS = (
    SampleSpec("sparse_4", 4, "low", 800, 1),
    SampleSpec("dense_9", 9, "high", 1200, 2),
    SampleSpec("burst_14", 14, "very_high", 1600, 3),
)


def _atomic_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(text.encode("utf-8"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except BaseException:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise


def _json_text(value: Any) -> str:
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            indent=2,
        )
        + "\n"
    )


def _make_segments(spec: SampleSpec, seed: int) -> list[dict[str, Any]]:
    rng = random.Random(seed + spec.seed_offset)
    envelopes = [
        Envelope.RECTANGULAR,
        Envelope.LINEAR_DECAY,
        Envelope.SINE,
    ]
    segments: list[dict[str, Any]] = []
    max_duration = min(260, spec.total_duration_ms // 3)
    min_duration = 45
    for index in range(spec.segment_count):
        duration = rng.randint(min_duration, max_duration)
        if spec.overlap == "low":
            slot = spec.total_duration_ms / spec.segment_count
            start = int(index * slot + rng.uniform(0, max(1.0, slot * 0.25)))
        elif spec.overlap == "high":
            start = int(
                index * (spec.total_duration_ms - duration) / spec.segment_count
                + rng.uniform(0, max(1.0, duration * 0.2))
            )
        else:
            cluster = index // 2
            start = int(
                cluster * (spec.total_duration_ms - duration) / (spec.segment_count / 2)
                + rng.uniform(0, max(1.0, duration * 0.35))
            )
        start = max(0, min(start, spec.total_duration_ms - duration))
        frequency = round(rng.uniform(24.0, 180.0), 3)
        amplitude = round(rng.uniform(0.3, 1.0), 4)
        segments.append(
            {
                "start_ms": start,
                "duration_ms": duration,
                "frequency_hz": frequency,
                "amplitude": amplitude,
                "envelope": envelopes[index % len(envelopes)].value,
            }
        )
    return segments


def config_specs() -> list[dict[str, Any]]:
    return [
        {
            "label": "balanced_hard",
            "config": {
                "sample_rate": 1000,
                "bit_depth": 16,
                "allocation_ratio": {"left": 0.5, "right": 0.5},
                "polarity": {"left": 1, "right": 1},
                "limiter": "hard",
            },
        },
        {
            "label": "left_bias_inverted",
            "config": {
                "sample_rate": 1000,
                "bit_depth": 16,
                "allocation_ratio": {"left": 0.72, "right": 0.28},
                "polarity": {"left": 1, "right": -1},
                "limiter": "hard",
            },
        },
        {
            "label": "high_rate_soft",
            "config": {
                "sample_rate": 2000,
                "bit_depth": 12,
                "allocation_ratio": {"left": 0.45, "right": 0.55},
                "polarity": {"left": -1, "right": 1},
                "limiter": "soft",
            },
        },
    ]


def generate_samples(
    output_dir: str | os.PathLike[str],
    *,
    seed: int = DEFAULT_SEED,
) -> dict[str, Any]:
    target = Path(output_dir)
    target.mkdir(parents=True, exist_ok=True)
    manifest_files: list[dict[str, Any]] = []
    for spec in SAMPLE_SPECS:
        segments = _make_segments(spec, seed)
        wave_payload = {
            "schema_version": 1,
            "sequence_id": spec.sample_id,
            "total_duration_ms": spec.total_duration_ms,
            "segments": segments,
        }
        sequence = WaveSequence.from_mapping(wave_payload)
        configs = config_specs()
        baseline_versions: list[dict[str, Any]] = []
        for index, item in enumerate(configs, start=1):
            config = DeviceConfig.from_mapping(item["config"])
            result = compute_full_signal(sequence, config)
            baseline_versions.append(
                {
                    "version": index,
                    "label": item["label"],
                    "config_hash": config.content_hash,
                    "signal_hash": result.signal_hash,
                    "left_sha256": bytes_hash(result.left_bytes),
                    "right_sha256": bytes_hash(result.right_bytes),
                    "total_frames": result.total_frames,
                    "limit_stats": result.limit_stats.to_dict(),
                    "segment_hashes": [
                        {
                            "index": signal.index,
                            "start_sample": signal.start_sample,
                            "end_sample": signal.end_sample,
                            "content_hash": signal.content_hash,
                        }
                        for signal in result.segment_signals
                    ],
                }
            )
        wave_path = target / f"{spec.sample_id}_wave.json"
        config_path = target / f"{spec.sample_id}_devices.json"
        baseline_path = target / f"{spec.sample_id}_baseline.json"
        device_payload = {
            "sample_id": spec.sample_id,
            "versions": [
                {"version": index, **item}
                for index, item in enumerate(configs, start=1)
            ],
        }
        baseline_payload = {
            "sample_id": spec.sample_id,
            "seed": seed,
            "wave_hash": sequence.content_hash,
            "versions": baseline_versions,
        }
        _atomic_text(wave_path, _json_text(wave_payload))
        _atomic_text(config_path, _json_text(device_payload))
        _atomic_text(baseline_path, _json_text(baseline_payload))
        manifest_files.append(
            {
                "sample_id": spec.sample_id,
                "segment_count": spec.segment_count,
                "overlap": spec.overlap,
                "wave": wave_path.name,
                "devices": config_path.name,
                "baseline": baseline_path.name,
                "wave_hash": sequence.content_hash,
                "baseline_hashes": [item["signal_hash"] for item in baseline_versions],
            }
        )
    manifest = {
        "schema_version": 1,
        "generator": "haptic_studio.samples",
        "seed": seed,
        "samples": manifest_files,
    }
    manifest_path = target / "manifest.json"
    _atomic_text(manifest_path, _json_text(manifest))
    return manifest


def load_wave_file(path: str | os.PathLike[str]) -> WaveSequence:
    try:
        payload = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise HapticError(
            ErrorCode.IO,
            f"cannot load wave file {path}",
            details={"error": str(exc)},
        ) from exc
    return WaveSequence.from_mapping(payload)


def load_config_file(
    path: str | os.PathLike[str],
) -> list[tuple[int, str, DeviceConfig]]:
    try:
        payload = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise HapticError(
            ErrorCode.IO,
            f"cannot load device config file {path}",
            details={"error": str(exc)},
        ) from exc
    if isinstance(payload, Mapping) and "versions" in payload:
        raw_versions = payload["versions"]
    elif isinstance(payload, list):
        raw_versions = payload
    else:
        raw_versions = [{"version": 1, "label": "v1", "config": payload}]
    if not isinstance(raw_versions, list) or not raw_versions:
        raise HapticError(
            ErrorCode.CONFIG_INVALID,
            "device config file must contain a non-empty versions array",
            field="versions",
        )
    result: list[tuple[int, str, DeviceConfig]] = []
    for index, item in enumerate(raw_versions, start=1):
        if not isinstance(item, Mapping):
            raise HapticError(
                ErrorCode.CONFIG_INVALID,
                "device config version must be an object",
                field="versions",
            )
        version = int(item.get("version", index))
        label = str(item.get("label", f"v{version}"))
        raw_config = item.get("config", item)
        result.append((version, label, DeviceConfig.from_mapping(raw_config)))
    return result


def export_csv(
    path: str | os.PathLike[str],
    left: Iterable[int],
    right: Iterable[int],
    *,
    sample_rate: int,
    limited: Iterable[int] | None = None,
) -> None:
    lines = ["sample_index,timestamp_ms,left,right,limited"]
    if limited is None:
        limited_values: Iterable[int] = (0 for _ in left)
    else:
        limited_values = limited
    for index, (left_value, right_value, limited_value) in enumerate(
        zip(left, right, limited_values, strict=True)
    ):
        timestamp_ms = index * 1000.0 / sample_rate
        lines.append(
            f"{index},{timestamp_ms:.6f},{int(left_value)},{int(right_value)},{int(limited_value)}"
        )
    _atomic_text(Path(path), "\n".join(lines) + "\n")
