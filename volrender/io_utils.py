"""Deterministic JSON/JSONL output and atomic artifact replacement."""

from __future__ import annotations

import io
import hashlib
import json
import os
import struct
import tempfile
import zlib
from pathlib import Path
from typing import Any

import numpy as np

from .renderer import RenderResult


def json_bytes(value: Any, pretty: bool = True) -> bytes:
    if pretty:
        text = json.dumps(
            value,
            sort_keys=True,
            indent=2,
            ensure_ascii=False,
            allow_nan=False,
        )
    else:
        text = json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        )
    return (text + "\n").encode("utf-8")


def jsonl_bytes(records: list[dict[str, Any]]) -> bytes:
    lines = [
        json.dumps(
            record,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        )
        for record in records
    ]
    return ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=target.parent
    )
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
    except Exception:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def npy_bytes(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, np.ascontiguousarray(array), allow_pickle=False)
    return buffer.getvalue()


def _png_chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    )


def png_bytes_from_ppm(ppm: bytes) -> bytes:
    """Convert the renderer's binary PPM to a deterministic PNG."""

    first = ppm.find(b"\n")
    second = ppm.find(b"\n", first + 1)
    third = ppm.find(b"\n", second + 1)
    width, height = (int(part) for part in ppm[first + 1 : second].split())
    pixels = ppm[third + 1 :]
    stride = width * 3
    if len(pixels) != stride * height:
        raise ValueError("invalid PPM payload length")
    raw = b"".join(
        b"\x00" + pixels[row * stride : (row + 1) * stride]
        for row in range(height)
    )
    header = b"\x89PNG\r\n\x1a\n" + _png_chunk(
        b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    )
    return (
        header
        + _png_chunk(b"IDAT", zlib.compress(raw, level=9))
        + _png_chunk(b"IEND", b"")
    )


def write_render_artifacts(result: RenderResult, output_dir: str | Path) -> dict[str, Any]:
    """Write render artifacts only after a complete render succeeds."""

    directory = Path(output_dir)
    directory.mkdir(parents=True, exist_ok=True)
    artifacts = {
        "image.ppm": result.image_ppm,
        "image.png": png_bytes_from_ppm(result.image_ppm),
        "summary.json": json_bytes(result.summary),
        "trace.jsonl": jsonl_bytes(result.trace),
        "scatter.npy": npy_bytes(result.scatter),
        "transmittance.npy": npy_bytes(result.transmittance),
    }
    if result.precision_delta is not None:
        artifacts["precision_delta.npy"] = npy_bytes(result.precision_delta)
    for name, payload in artifacts.items():
        atomic_write_bytes(directory / name, payload)
    manifest = {
        "schema": "volrender.artifacts.v1",
        "files": {
            name: {
                "bytes": len(payload),
                "sha256": hashlib.sha256(payload).hexdigest(),
            }
            for name, payload in sorted(artifacts.items())
        },
        "pixel_hash": result.pixel_hash,
        "image_hash": result.image_hash,
        "summary_hash": result.summary["summary_hash"],
    }
    atomic_write_bytes(directory / "manifest.json", json_bytes(manifest))
    return manifest
