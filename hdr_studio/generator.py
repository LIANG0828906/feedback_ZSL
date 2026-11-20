from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .color import encode_transfer
from .formats import build_header, expected_frame_bytes
from .model import ColorSpace, TransferFunction
from .util import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_bytes,
    hash_file,
    sha256_hex,
)


@dataclass(frozen=True, slots=True)
class SampleSpec:
    name: str
    width: int
    height: int
    frame_count: int
    bit_depth: int
    color_space: ColorSpace
    transfer: TransferFunction
    peak_luminance: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "width": self.width,
            "height": self.height,
            "frame_count": self.frame_count,
            "bit_depth": self.bit_depth,
            "color_space": self.color_space.value,
            "transfer": self.transfer.value,
            "peak_luminance": self.peak_luminance,
        }


DEFAULT_SAMPLES: tuple[SampleSpec, ...] = (
    SampleSpec(
        name="rec2020_pq_10bit",
        width=24,
        height=14,
        frame_count=7,
        bit_depth=10,
        color_space=ColorSpace.REC2020,
        transfer=TransferFunction.PQ,
        peak_luminance=1000.0,
    ),
    SampleSpec(
        name="rec709_sdr_10bit",
        width=18,
        height=11,
        frame_count=5,
        bit_depth=10,
        color_space=ColorSpace.REC709,
        transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
    ),
    SampleSpec(
        name="p3_hlg_12bit",
        width=15,
        height=9,
        frame_count=4,
        bit_depth=12,
        color_space=ColorSpace.DISPLAY_P3,
        transfer=TransferFunction.HLG,
        peak_luminance=600.0,
    ),
    SampleSpec(
        name="rec2020_pq_16bit",
        width=10,
        height=6,
        frame_count=3,
        bit_depth=16,
        color_space=ColorSpace.REC2020,
        transfer=TransferFunction.PQ,
        peak_luminance=4000.0,
    ),
)


def _linear_pattern(spec: SampleSpec, frame_index: int, rng: np.random.Generator) -> np.ndarray:
    height, width = spec.height, spec.width
    yy, xx = np.mgrid[0:height, 0:width]
    x = xx.astype(np.float64) / max(1, width - 1)
    y = yy.astype(np.float64) / max(1, height - 1)
    phase = frame_index / max(1, spec.frame_count - 1)
    red = 0.08 + 0.78 * x + 0.08 * np.sin(6.0 * y + 4.0 * phase)
    green = 0.08 + 0.72 * y + 0.10 * np.cos(5.0 * x - 3.0 * phase)
    blue = 0.10 + 0.62 * (1.0 - x) * (0.35 + 0.65 * y) + 0.14 * phase
    noise = rng.uniform(-0.015, 0.015, size=(height, width, 3))
    linear = np.stack(
        (
            np.broadcast_to(red, (height, width)),
            np.broadcast_to(green, (height, width)),
            np.broadcast_to(blue, (height, width)),
        ),
        axis=-1,
    )
    return np.clip(linear + noise, 0.0, 1.0)


def _encode_sample_frame(
    linear: np.ndarray, spec: SampleSpec
) -> np.ndarray:
    nits = linear * spec.peak_luminance
    encoded = encode_transfer(nits, spec.transfer, spec.peak_luminance)
    max_code = (1 << spec.bit_depth) - 1
    codes = np.rint(np.clip(encoded * max_code, 0.0, max_code)).astype(np.uint16)
    return codes


def generate_sample(spec: SampleSpec, output_dir: Path, seed: int) -> dict[str, Any]:
    rng = np.random.default_rng(seed)
    header = build_header(
        width=spec.width,
        height=spec.height,
        frame_count=spec.frame_count,
        bit_depth=spec.bit_depth,
        color_space=spec.color_space,
        transfer=spec.transfer,
        peak_luminance=spec.peak_luminance,
    )
    frame_bytes = expected_frame_bytes(spec.width, spec.height, spec.bit_depth)
    frame_hashes: list[str] = []
    chunks = [header]
    for frame_index in range(spec.frame_count):
        codes = _encode_sample_frame(_linear_pattern(spec, frame_index, rng), spec)
        if spec.bit_depth <= 8:
            raw = codes.astype(np.uint8).tobytes(order="C")
        else:
            raw = codes.astype("<u2").tobytes(order="C")
        if len(raw) != frame_bytes:
            raise RuntimeError("generated frame length disagreed with header")
        frame_hashes.append(sha256_hex(raw))
        chunks.append(raw)
    data = b"".join(chunks)
    output_dir.mkdir(parents=True, exist_ok=True)
    path = output_dir / f"{spec.name}.hdrseq"
    atomic_write_bytes(path, data)
    return {
        **spec.to_dict(),
        "file": path.name,
        "header_size": len(header),
        "header_hash": sha256_hex(header),
        "file_hash": hash_file(path),
        "frame_hashes": frame_hashes,
    }


def generate_sample_set(
    workspace: Path | str,
    *,
    seed: int = 20261121,
    specs: tuple[SampleSpec, ...] = DEFAULT_SAMPLES,
) -> dict[str, Any]:
    workspace = Path(workspace)
    samples_dir = workspace / "samples"
    samples_dir.mkdir(parents=True, exist_ok=True)
    entries = []
    for index, spec in enumerate(specs):
        entries.append(generate_sample(spec, samples_dir, seed + index * 104729))
    manifest = {
        "schema_version": 1,
        "generator": "hdr_studio.generator",
        "seed": seed,
        "samples": entries,
    }
    manifest["sample_set_hash"] = sha256_hex(canonical_bytes(entries))
    atomic_write_json(samples_dir / "manifest.json", manifest)
    return manifest


def load_sample_manifest(workspace: Path | str) -> dict[str, Any] | None:
    path = Path(workspace) / "samples" / "manifest.json"
    if not path.exists():
        return None
    import json

    return json.loads(path.read_text("utf-8"))

