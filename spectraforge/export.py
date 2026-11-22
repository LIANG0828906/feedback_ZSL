"""Deterministic ordered export of a fully committed composite."""

from __future__ import annotations

import io
import struct
import zlib
from pathlib import Path
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .models import CompositeOutput
from .util import atomic_write_bytes, atomic_write_json, canonical_json_bytes, sha256_bytes


def _png_chunk(kind: bytes, data: bytes) -> bytes:
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)


def encode_png_rgb(display: np.ndarray) -> bytes:
    rgb = np.asarray(display, dtype=np.uint8)
    if rgb.ndim != 3 or rgb.shape[2] != 3:
        raise WorkbenchError("E_DIM", "PNG export requires H x W x 3 RGB data", {"shape": list(rgb.shape)})
    height, width, _ = rgb.shape
    raw = b"".join(b"\x00" + rgb[row].tobytes(order="C") for row in range(height))
    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _png_chunk(b"IHDR", header)
        + _png_chunk(b"IDAT", zlib.compress(raw, level=9))
        + _png_chunk(b"IEND", b"")
    )


def encode_npy(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array), allow_pickle=False)
    return buffer.getvalue()


def _json_ready(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _json_ready(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_ready(item) for item in value]
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        return value.tolist()
    return value


def export_output(
    output: CompositeOutput,
    output_dir: str | Path,
    *,
    scene_id: str,
    title: str,
    segment_order: list[int] | None = None,
    alignment: Any | None = None,
) -> dict[str, Any]:
    if not output.complete:
        raise WorkbenchError(
            "E_INCOMPLETE",
            "Only a fully committed composite may be exported",
            {"segments": sorted(output.segment_stats)},
        )
    target = Path(output_dir).resolve()
    target.mkdir(parents=True, exist_ok=True)
    order = segment_order if segment_order is not None else sorted(output.segment_stats)
    files: dict[str, str] = {}
    payloads = {
        "display.png": encode_png_rgb(output.display),
        "radiance.npy": encode_npy(output.radiance),
        "linear_rgb.npy": encode_npy(output.linear_rgb),
        "rgb_chroma.npy": encode_npy(output.rgb_chroma),
        "gamut_channel_mask.npy": encode_npy(output.gamut_channel_mask),
    }
    if alignment is not None:
        payloads["aligned_bands.npy"] = encode_npy(alignment.aligned_bands)
        payloads["alignment_report.json"] = canonical_json_bytes(alignment.alignment_report) + b"\n"
    for name, payload in payloads.items():
        atomic_write_bytes(target / name, payload)
        files[name] = sha256_bytes(payload)
    stats = {
        "scene_id": scene_id,
        "title": title,
        "complete": True,
        "segment_order": order,
        "summary": output.summary(),
        "export_files": files,
        "export_contract": "segments assembled in ascending stable segment index",
    }
    atomic_write_json(target / "stats.json", _json_ready(stats))
    files["stats.json"] = sha256_bytes(canonical_json_bytes(_json_ready(stats)) + b"\n")
    manifest = {
        "format": "spectraforge-export-v1",
        "scene_id": scene_id,
        "title": title,
        "complete": True,
        "metadata_version": output.metadata_version,
        "metadata_hash": output.metadata_hash,
        "algorithm_version": output.algorithm_version,
        "param_versions": output.param_versions,
        "segment_order": order,
        "files": files,
        "display_size": [int(output.display.shape[1]), int(output.display.shape[0])],
    }
    atomic_write_json(target / "export_manifest.json", manifest)
    return manifest
