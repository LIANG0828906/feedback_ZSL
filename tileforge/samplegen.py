"""Deterministic local sample and baseline generator."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from .binary import atomic_write_bytes, make_block, pack_samples, write_sequence_atomic
from .config import ColorSpaceConfig, JobConfig, MemoryConfig, ObservationSpec
from .model import Primaries, SequenceHeader, TileSequence, TransferCode


MASK64 = (1 << 64) - 1


def _splitmix64(value: int) -> int:
    value = (value + 0x9E3779B97F4A7C15) & MASK64
    value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & MASK64
    return value ^ (value >> 31)


def _texture_value(seed: int, tile_id: int, x: int, y: int, channel: int, width: int, height: int, max_code: int) -> int:
    noise = (_splitmix64(seed ^ (tile_id << 48) ^ (x << 24) ^ (y << 8) ^ channel) >> 32) & 0xFF
    gx = (x * 255) // max(1, width - 1)
    gy = (y * 255) // max(1, height - 1)
    diagonal = ((x + y + channel * 7) * 255) // max(1, width + height + 14)
    smooth = (gx * 3 + gy * 2 + diagonal * 2) // 7
    mixed = (smooth * 5 + noise * 3) // 8
    return (mixed * max_code + 127) // 255


SAMPLE_SPECS: tuple[dict[str, Any], ...] = (
    {
        "name": "texture_8bit_rec709_sdr",
        "width": 64,
        "height": 48,
        "tiles_x": 4,
        "tiles_y": 3,
        "bit_depth": 8,
        "source_primaries": Primaries.REC709,
        "source_transfer": TransferCode.SDR_GAMMA,
        "target_primaries": Primaries.DISPLAY_P3,
        "target_transfer": TransferCode.PQ,
        "source_peak": 100.0,
        "target_peak": 1000.0,
        "curve": "reinhard",
    },
    {
        "name": "texture_10bit_p3_pq",
        "width": 80,
        "height": 48,
        "tiles_x": 5,
        "tiles_y": 3,
        "bit_depth": 10,
        "source_primaries": Primaries.DISPLAY_P3,
        "source_transfer": TransferCode.PQ,
        "target_primaries": Primaries.REC2020,
        "target_transfer": TransferCode.HLG,
        "source_peak": 1000.0,
        "target_peak": 1000.0,
        "curve": "aces",
    },
    {
        "name": "texture_12bit_rec2020_hlg",
        "width": 96,
        "height": 72,
        "tiles_x": 6,
        "tiles_y": 4,
        "bit_depth": 12,
        "source_primaries": Primaries.REC2020,
        "source_transfer": TransferCode.HLG,
        "target_primaries": Primaries.DISPLAY_P3,
        "target_transfer": TransferCode.SDR_GAMMA,
        "source_peak": 1000.0,
        "target_peak": 100.0,
        "curve": "reinhard",
    },
    {
        "name": "texture_16bit_rec2020_pq",
        "width": 64,
        "height": 64,
        "tiles_x": 4,
        "tiles_y": 4,
        "bit_depth": 16,
        "source_primaries": Primaries.REC2020,
        "source_transfer": TransferCode.PQ,
        "target_primaries": Primaries.REC709,
        "target_transfer": TransferCode.SDR_GAMMA,
        "source_peak": 1000.0,
        "target_peak": 100.0,
        "curve": "none",
    },
)


def sample_config(spec: dict[str, Any]) -> JobConfig:
    return JobConfig(
        source=ColorSpaceConfig(
            primaries=spec["source_primaries"],
            transfer=spec["source_transfer"],
            white_point="d65",
            peak_nits=spec["source_peak"],
        ),
        target=ColorSpaceConfig(
            primaries=spec["target_primaries"],
            transfer=spec["target_transfer"],
            white_point="d65",
            peak_nits=spec["target_peak"],
        ),
        tone_map_curve=spec["curve"],
        observation=ObservationSpec(window=None, sync_marker="sync-sample-01", tolerance_code=1),
        memory=MemoryConfig(
            parallelism=2,
            tile_budget_bytes=1024 * 1024,
            run_budget_bytes=4 * 1024 * 1024,
            downgrade_profiles=("full", "banded", "streaming"),
        ),
        preview_refresh_ms=200,
    )


def generate_sequence(spec: dict[str, Any], seed: int) -> TileSequence:
    width = int(spec["width"])
    height = int(spec["height"])
    tiles_x = int(spec["tiles_x"])
    tiles_y = int(spec["tiles_y"])
    bit_depth = int(spec["bit_depth"])
    if width % tiles_x or height % tiles_y:
        raise ValueError(f"sample grid is invalid for {spec['name']}")
    max_code = (1 << bit_depth) - 1
    tile_width = width // tiles_x
    tile_height = height // tiles_y
    config = sample_config(spec)
    header = SequenceHeader(
        width=width,
        height=height,
        tiles_x=tiles_x,
        tiles_y=tiles_y,
        bit_depth=bit_depth,
        channels=3,
        source_transfer=spec["source_transfer"],
        target_transfer=spec["target_transfer"],
        conversion_id=config.conversion_id(),
        block_count=tiles_x * tiles_y,
        data_length=tile_width * tile_height * 3 * (1 if bit_depth == 8 else 2) * tiles_x * tiles_y,
    )
    blocks = []
    for tile_id in range(tiles_x * tiles_y):
        x0 = (tile_id % tiles_x) * tile_width
        y0 = (tile_id // tiles_x) * tile_height
        values: list[int] = []
        for local_y in range(tile_height):
            for local_x in range(tile_width):
                for channel in range(3):
                    values.append(
                        _texture_value(
                            seed,
                            tile_id,
                            x0 + local_x,
                            y0 + local_y,
                            channel,
                            width,
                            height,
                            max_code,
                        )
                    )
        exposure = (((_splitmix64(seed + tile_id * 97) >> 40) % 121) - 60) / 100.0
        blocks.append(
            make_block(
                tile_id,
                x0,
                y0,
                tile_width,
                tile_height,
                exposure,
                3,
                pack_samples(values, bit_depth),
            )
        )
    return TileSequence(header=header, blocks=blocks)


def generate_samples(root: str | Path, seed: int = 190) -> dict[str, Any]:
    output = Path(root)
    output.mkdir(parents=True, exist_ok=True)
    entries: list[dict[str, Any]] = []
    baselines: dict[str, str] = {}
    for spec in SAMPLE_SPECS:
        sequence = generate_sequence(spec, seed)
        name = str(spec["name"])
        sequence_path = output / f"{name}.tcseq"
        config_path = output / f"{name}.config.json"
        version_hash = write_sequence_atomic(sequence_path, sequence)
        config = sample_config(spec)
        atomic_write_bytes(
            config_path,
            json.dumps(config.to_dict(), sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n",
        )
        baselines[sequence_path.name] = version_hash
        entries.append(
            {
                "name": name,
                "sequence": sequence_path.name,
                "config": config_path.name,
                "sha256": version_hash,
                "header": {
                    "width": sequence.header.width,
                    "height": sequence.header.height,
                    "tiles_x": sequence.header.tiles_x,
                    "tiles_y": sequence.header.tiles_y,
                    "bit_depth": sequence.header.bit_depth,
                    "source_transfer": sequence.header.source_transfer.name,
                    "target_transfer": sequence.header.target_transfer.name,
                },
                "tile_hashes": {str(block.tile_id): block.content_hash.hex() for block in sequence.blocks},
            }
        )
    manifest = {
        "schema": 1,
        "generator": "tileforge.samplegen",
        "seed": seed,
        "samples": entries,
        "baseline_hashes": baselines,
        "manifest_hash": hashlib.sha256(
            json.dumps(
                {"seed": seed, "samples": entries, "baseline_hashes": baselines},
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
        ).hexdigest(),
    }
    atomic_write_bytes(
        output / "baseline.json",
        json.dumps(manifest, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n",
    )
    return manifest
