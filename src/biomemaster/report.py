"""Fixed-precision JSON reports and report/WAV consistency verification."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .constants import REPORT_DECIMALS, TRUE_PEAK_CEILING_DBTP
from .errors import RenderFailed
from .loudness import (
    initial_loudness_state,
    integrated_loudness_from_ledger,
    process_loudness_chunk,
)
from .storage import atomic_write_bytes
from .truepeak import true_peak_dbtp_wav

REPORT_SCHEMA_VERSION = 1


def _encode_fixed(value: Any, level: int = 0) -> str:
    indent = "  " * level
    child_indent = "  " * (level + 1)
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        if not math.isfinite(value):
            raise RenderFailed("QC report contains a non-finite float")
        return f"{value:.{REPORT_DECIMALS}f}"
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, list):
        if not value:
            return "[]"
        items = [f"{child_indent}{_encode_fixed(item, level + 1)}" for item in value]
        return "[\n" + ",\n".join(items) + f"\n{indent}]"
    if isinstance(value, dict):
        if not value:
            return "{}"
        items = []
        for key in sorted(value):
            encoded_key = json.dumps(str(key), ensure_ascii=False)
            items.append(
                f"{child_indent}{encoded_key}: {_encode_fixed(value[key], level + 1)}"
            )
        return "{\n" + ",\n".join(items) + f"\n{indent}}}"
    raise TypeError(f"cannot encode {type(value).__name__} as fixed JSON")


def fixed_json_text(value: Any) -> str:
    return _encode_fixed(value) + "\n"


def write_report(path: Path, report: dict[str, Any]) -> None:
    payload = fixed_json_text(report).encode("utf-8")
    atomic_write_bytes(path, payload)


def sha256_path(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while True:
            block = handle.read(1024 * 1024)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def measure_wav_loudness(path: Path) -> tuple[float, int, int, int]:
    with sf.SoundFile(str(path)) as handle:
        sample_rate = handle.samplerate
        channels = handle.channels
        total = len(handle)
        state = initial_loudness_state(sample_rate, channels)
        energies: list[tuple[float, float]] = []
        while True:
            block = handle.read(65536, dtype="float64", always_2d=True)
            if block.shape[0] == 0:
                break
            state, records = process_loudness_chunk(state, block)
            energies.extend(records)
    records_array = (
        np.asarray(energies, dtype=np.float64)
        if energies
        else np.zeros((0, 2), dtype=np.float64)
    )
    loudness = integrated_loudness_from_ledger(records_array, state)
    return loudness, total, sample_rate, channels


def verify_report(report_path: Path, wav_path: Path) -> dict[str, Any]:
    try:
        report = json.loads(report_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise RenderFailed("QC report cannot be parsed", error=str(exc)) from exc
    if report.get("schema_version") != REPORT_SCHEMA_VERSION:
        raise RenderFailed("unsupported QC report schema")
    if not wav_path.is_file():
        raise RenderFailed("WAV file does not exist", path=str(wav_path))

    actual_sha256 = sha256_path(wav_path)
    actual_size = wav_path.stat().st_size
    if report.get("wav_sha256") != actual_sha256:
        raise RenderFailed(
            "WAV SHA-256 does not match QC report",
            expected=report.get("wav_sha256"),
            actual=actual_sha256,
        )
    if int(report.get("wav_size_bytes", -1)) != actual_size:
        raise RenderFailed("WAV size does not match QC report")

    loudness, total, sample_rate, channels = measure_wav_loudness(wav_path)
    true_peak = true_peak_dbtp_wav(wav_path)
    info = sf.info(str(wav_path))
    checks = {
        "total_samples": int(report["total_samples"]) == total,
        "sample_rate": int(report["sample_rate"]) == sample_rate,
        "output_channels": int(report["output_channels"]) == channels,
        "subtype": report.get("wav_subtype") == info.subtype,
        "integrated_loudness": abs(float(report["integrated_loudness_lufs"]) - loudness)
        <= 1e-5,
        "true_peak": abs(float(report["true_peak_dbtp"]) - true_peak) <= 1e-5,
        "true_peak_ceiling": true_peak <= TRUE_PEAK_CEILING_DBTP + 1e-6,
    }
    if not all(checks.values()):
        raise RenderFailed("QC report is inconsistent with WAV", checks=checks)
    return {
        "ok": True,
        "wav_sha256": actual_sha256,
        "total_samples": total,
        "integrated_loudness_lufs": round(loudness, REPORT_DECIMALS),
        "true_peak_dbtp": round(true_peak, REPORT_DECIMALS),
        "checks": checks,
    }
