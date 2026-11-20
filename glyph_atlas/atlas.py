"""Deterministic shelf packing and output hashes."""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .config import AtlasConfig, TargetConfig
from .errors import E_ATLAS_OVERFLOW, E_SNAPSHOT_MISMATCH, fail
from .png import encode_pgm, write_rgba_png
from .scheduler import GlyphRasterResult
from .snapshot import atomic_write_bytes, atomic_write_json, canonical_json_bytes


@dataclass(frozen=True)
class AtlasBuildResult:
    target: TargetConfig
    output_dir: Path
    atlas_path: Path
    metrics_path: Path
    manifest_path: Path
    atlas_sha256: str
    metrics_sha256: str
    source_coverage_sha256: str
    manifest: dict[str, Any]

    def outputs_section(self) -> dict[str, Any]:
        return {
            "coverage_sha256": self.source_coverage_sha256,
            "atlas_sha256": self.atlas_sha256,
            "metrics_sha256": self.metrics_sha256,
            "atlas_file": self.atlas_path.name,
            "metrics_file": self.metrics_path.name,
            "manifest_file": self.manifest_path.name,
        }


def build_atlas(
    glyphs: Iterable[GlyphRasterResult],
    target: TargetConfig,
    atlas_config: AtlasConfig,
    output_dir: str | Path,
    temp_dir: str | Path,
) -> AtlasBuildResult:
    glyph_list = list(glyphs)
    if not glyph_list:
        fail(E_ATLAS_OVERFLOW, "No glyphs were supplied to the atlas", glyphs=[])
    placements: list[dict[str, Any]] = []
    unplaced: list[str] = []
    cursor_x = atlas_config.padding
    cursor_y = atlas_config.padding
    shelf_height = 0
    usable_width = atlas_config.width - atlas_config.padding
    usable_height = atlas_config.height - atlas_config.padding

    for glyph in glyph_list:
        width = glyph.plan.width
        height = glyph.plan.height
        if width > atlas_config.width - 2 * atlas_config.padding or height > atlas_config.height - 2 * atlas_config.padding:
            unplaced.append(glyph.glyph_name)
            continue
        if cursor_x + width > usable_width:
            cursor_x = atlas_config.padding
            cursor_y += shelf_height + atlas_config.padding
            shelf_height = 0
        if cursor_y + height > usable_height:
            unplaced.append(glyph.glyph_name)
            continue
        x = cursor_x
        y = cursor_y
        if x < 0 or y < 0 or x + width > atlas_config.width or y + height > atlas_config.height:
            fail(
                E_ATLAS_OVERFLOW,
                "Glyph metrics lie outside the atlas",
                glyph=glyph.glyph_name,
                atlas=[atlas_config.width, atlas_config.height],
                rect=[x, y, width, height],
            )
        placements.append(
            {
                "glyph": glyph.glyph_name,
                "atlas_rect": [x, y, width, height],
                "advance": _round(glyph.plan.advance_px),
                "bearing_x": glyph.plan.origin_x,
                "bearing_y": glyph.plan.origin_top,
                "bbox": [_round(value) for value in glyph.plan.bbox],
                "unicode": list(glyph.unicode),
                "fill_rule": glyph.plan.fill_rule,
                "coverage_sha256": glyph.coverage_hash,
                "snapshot_head": glyph.tiles[-1].chain_hash if glyph.tiles else None,
                "coverage_file": _coverage_filename(glyph),
            }
        )
        cursor_x += width + atlas_config.padding
        shelf_height = max(shelf_height, height)

    if unplaced:
        fail(
            E_ATLAS_OVERFLOW,
            "Atlas has insufficient space",
            glyphs=unplaced,
            unplaced_glyphs=unplaced,
            atlas=[atlas_config.width, atlas_config.height],
            padding=atlas_config.padding,
        )

    atlas_rgba = np.zeros((atlas_config.height, atlas_config.width, 4), dtype=np.uint8)
    atlas_rgba[:, :, :3] = 255
    by_name = {glyph.glyph_name: glyph for glyph in glyph_list}
    for placement in placements:
        glyph = by_name[placement["glyph"]]
        x, y, width, height = placement["atlas_rect"]
        atlas_rgba[y : y + height, x : x + width, 3] = glyph.coverage

    source_hash = combined_coverage_hash(glyph_list)
    metrics = {
        "format": "glyph-atlas-metrics/1",
        "target": target.as_dict(),
        "atlas": {
            "width": atlas_config.width,
            "height": atlas_config.height,
            "padding": atlas_config.padding,
        },
        "source_coverage_sha256": source_hash,
        "glyphs": placements,
    }
    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)
    atlas_path = output_path / "atlas.png"
    metrics_path = output_path / "metrics.json"
    manifest_path = output_path / "manifest.json"
    atlas_bytes = write_rgba_png(atlas_path, atlas_rgba, temp_dir=temp_dir)
    atlas_sha = hashlib.sha256(atlas_bytes).hexdigest()
    coverage_hashes: dict[str, str] = {}
    for placement in placements:
        glyph = by_name[placement["glyph"]]
        relative_file = placement["coverage_file"]
        coverage_path = output_path / relative_file
        coverage_bytes = encode_pgm(glyph.coverage)
        atomic_write_bytes(coverage_path, coverage_bytes, temp_dir=temp_dir)
        coverage_hashes[relative_file] = hashlib.sha256(coverage_bytes).hexdigest()
    metrics_bytes = canonical_json_bytes(metrics) + b"\n"
    metrics_sha = hashlib.sha256(metrics_bytes).hexdigest()
    atomic_write_bytes(metrics_path, metrics_bytes, temp_dir=temp_dir)
    manifest = {
        "format": "glyph-atlas-manifest/1",
        "target": target.as_dict(),
        "source_coverage_sha256": source_hash,
        "atlas_sha256": atlas_sha,
        "metrics_sha256": metrics_sha,
        "glyph_count": len(glyph_list),
        "glyph_snapshot_heads": {
            glyph.glyph_name: glyph.tiles[-1].chain_hash if glyph.tiles else None
            for glyph in glyph_list
        },
        "coverage_files_sha256": coverage_hashes,
    }
    atomic_write_json(manifest_path, manifest, temp_dir=temp_dir)
    return AtlasBuildResult(
        target=target,
        output_dir=output_path,
        atlas_path=atlas_path,
        metrics_path=metrics_path,
        manifest_path=manifest_path,
        atlas_sha256=atlas_sha,
        metrics_sha256=metrics_sha,
        source_coverage_sha256=source_hash,
        manifest=manifest,
    )


def combined_coverage_hash(glyphs: Iterable[GlyphRasterResult]) -> str:
    document = {
        glyph.glyph_name: glyph.coverage_hash
        for glyph in glyphs
    }
    return hashlib.sha256(canonical_json_bytes(document)).hexdigest()


def verify_output_hashes(output_dir: str | Path) -> dict[str, Any]:
    directory = Path(output_dir)
    try:
        manifest = json.loads((directory / "manifest.json").read_text(encoding="utf-8"))
        metrics = json.loads((directory / "metrics.json").read_text(encoding="utf-8"))
        metrics_bytes = (directory / "metrics.json").read_bytes()
        atlas_bytes = (directory / "atlas.png").read_bytes()
    except (OSError, json.JSONDecodeError) as exc:
        fail(E_SNAPSHOT_MISMATCH, "Output files cannot be verified", output_dir=str(directory), error=str(exc))
    metrics_sha = hashlib.sha256(metrics_bytes).hexdigest()
    atlas_sha = hashlib.sha256(atlas_bytes).hexdigest()
    if manifest.get("metrics_sha256") != metrics_sha or manifest.get("atlas_sha256") != atlas_sha:
        fail(
            E_SNAPSHOT_MISMATCH,
            "Output hash does not match manifest",
            output_dir=str(directory),
            expected={
                "atlas_sha256": manifest.get("atlas_sha256"),
                "metrics_sha256": manifest.get("metrics_sha256"),
            },
            actual={"atlas_sha256": atlas_sha, "metrics_sha256": metrics_sha},
        )
    source_hash = manifest.get("source_coverage_sha256")
    if metrics.get("source_coverage_sha256") != source_hash:
        fail(
            E_SNAPSHOT_MISMATCH,
            "Metrics source coverage hash does not match manifest",
            output_dir=str(directory),
        )
    coverage_hashes = {
        glyph.get("glyph"): glyph.get("coverage_sha256")
        for glyph in metrics.get("glyphs", [])
        if isinstance(glyph, dict)
    }
    recomputed = hashlib.sha256(canonical_json_bytes(coverage_hashes)).hexdigest()
    if recomputed != source_hash:
        fail(
            E_SNAPSHOT_MISMATCH,
            "Metrics coverage hashes do not reconstruct the source snapshot hash",
            output_dir=str(directory),
            expected=source_hash,
            actual=recomputed,
        )
    if manifest.get("glyph_count") != len(coverage_hashes):
        fail(E_SNAPSHOT_MISMATCH, "Manifest glyph count does not match metrics", output_dir=str(directory))
    file_hashes = manifest.get("coverage_files_sha256")
    if not isinstance(file_hashes, dict):
        fail(E_SNAPSHOT_MISMATCH, "Manifest coverage file hash table is missing", output_dir=str(directory))
    for relative_file, expected_hash in file_hashes.items():
        try:
            actual_hash = hashlib.sha256((directory / relative_file).read_bytes()).hexdigest()
        except OSError as exc:
            fail(
                E_SNAPSHOT_MISMATCH,
                "Coverage byte image is missing",
                output_dir=str(directory),
                file=relative_file,
                error=str(exc),
            )
        if actual_hash != expected_hash:
            fail(
                E_SNAPSHOT_MISMATCH,
                "Coverage byte image hash mismatch",
                output_dir=str(directory),
                file=relative_file,
            )
    return manifest


def _round(value: float) -> float:
    return float(round(value, 9))


def _coverage_filename(glyph: GlyphRasterResult) -> str:
    safe = re.sub(r"[^A-Za-z0-9_.-]+", "_", glyph.glyph_name)
    return f"coverage/{glyph.plan.glyph.index:04d}-{safe}.pgm"

