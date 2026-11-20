"""End-to-end deterministic rasterization and atlas pipeline."""

from __future__ import annotations

import hashlib
import json
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .atlas import AtlasBuildResult, build_atlas, verify_output_hashes
from .config import TargetConfig, WorkConfig, load_config
from .errors import GlyphAtlasError
from .font import FontSpec, load_font
from .scheduler import GlyphRasterResult, RasterScheduler, SchedulerReport
from .snapshot import (
    atomic_write_bytes,
    atomic_write_json,
    canonical_json_bytes,
    ensure_writable_directory,
)


@dataclass
class TargetRun:
    schedule: SchedulerReport
    atlas: AtlasBuildResult
    trace_path: Path
    trace_sha256: str


@dataclass
class PipelineReport:
    config: WorkConfig
    font: FontSpec
    targets: tuple[TargetRun, ...]
    baseline_path: Path
    baseline: dict[str, Any]

    def glyphs_for(self, target_name: str) -> dict[str, GlyphRasterResult]:
        for run in self.targets:
            if run.schedule.target.name == target_name:
                return run.schedule.glyph_map()
        raise KeyError(target_name)


def run_pipeline(
    config_path: str | Path,
    resume: bool = False,
    progress: Callable[[dict[str, Any]], None] | None = None,
    cancel_event: threading.Event | None = None,
) -> PipelineReport:
    config = load_config(config_path)
    font = load_font(config.font_path)
    return run_loaded(config, font, resume=resume, progress=progress, cancel_event=cancel_event)


def run_loaded(
    config: WorkConfig,
    font: FontSpec,
    resume: bool = False,
    progress: Callable[[dict[str, Any]], None] | None = None,
    cancel_event: threading.Event | None = None,
) -> PipelineReport:
    ensure_writable_directory(config.temp_dir)
    ensure_writable_directory(config.output_dir)
    ensure_writable_directory(config.snapshot_dir)
    glyph_specs = list(font.glyphs) if config.glyphs is None else [font.glyph(name) for name in config.glyphs]
    if not glyph_specs:
        from .errors import E_CONFIG_INVALID, fail

        fail(E_CONFIG_INVALID, "Configured glyph set is empty")

    target_runs: list[TargetRun] = []
    all_trace: list[dict[str, Any]] = []
    target_baselines: list[dict[str, Any]] = []
    for target in config.targets:
        scheduler = RasterScheduler(
            font=font,
            config=config,
            target=target,
            resume=resume,
            progress=progress,
            cancel_event=cancel_event,
        )
        for glyph_spec in glyph_specs:
            scheduler.submit_glyph(glyph_spec.name)
        target_output = config.output_dir / target.name
        try:
            schedule = scheduler.run()
        except GlyphAtlasError as exc:
            trace = scheduler.trace_records()
            trace_path = target_output / "trace.jsonl"
            trace_bytes = b"".join(canonical_json_bytes(record) + b"\n" for record in trace)
            atomic_write_bytes(trace_path, trace_bytes, temp_dir=config.temp_dir)
            exc.details.setdefault("trace_file", str(trace_path))
            raise
        all_trace.extend(schedule.trace)

        atlas = build_atlas(
            schedule.glyphs,
            target=target,
            atlas_config=config.atlas,
            output_dir=target_output,
            temp_dir=config.temp_dir,
        )
        verify_output_hashes(target_output)
        for glyph in schedule.glyphs:
            outputs = atlas.outputs_section()
            outputs["glyph_coverage_sha256"] = glyph.coverage_hash
            outputs["glyph_snapshot_head"] = glyph.tiles[-1].chain_hash if glyph.tiles else None
            glyph.snapshot_store.attach_outputs(outputs)

        trace_path = target_output / "trace.jsonl"
        trace_bytes = b"".join(canonical_json_bytes(record) + b"\n" for record in schedule.trace)
        atomic_write_bytes(trace_path, trace_bytes, temp_dir=config.temp_dir)
        trace_sha = hashlib.sha256(trace_bytes).hexdigest()
        baseline_entry = {
            "target": target.as_dict(),
            "run_descriptor_sha256": _run_descriptor_hash(config, font, target, glyph_specs),
            "glyphs": [
                {
                    **glyph.metrics_record(),
                    "snapshot": glyph.snapshot_path.name,
                    "snapshot_head": glyph.tiles[-1].chain_hash if glyph.tiles else None,
                    "whole_reference": glyph.whole_reference,
                }
                for glyph in schedule.glyphs
            ],
            "atlas": {
                "source_coverage_sha256": atlas.source_coverage_sha256,
                "atlas_sha256": atlas.atlas_sha256,
                "metrics_sha256": atlas.metrics_sha256,
            },
            "trace_sha256": trace_sha,
            "trace_file": f"{target.name}/trace.jsonl",
        }
        target_baselines.append(baseline_entry)
        target_runs.append(TargetRun(schedule=schedule, atlas=atlas, trace_path=trace_path, trace_sha256=trace_sha))

    combined_trace: list[dict[str, Any]] = []
    for sequence, record in enumerate(all_trace):
        combined_record = dict(record)
        combined_record["seq"] = sequence
        combined_trace.append(combined_record)
    combined_trace_bytes = b"".join(canonical_json_bytes(record) + b"\n" for record in combined_trace)
    combined_trace_path = config.output_dir / "trace.jsonl"
    atomic_write_bytes(combined_trace_path, combined_trace_bytes, temp_dir=config.temp_dir)
    baseline = {
        "format": "glyph-atlas-baseline/1",
        "font": {
            "name": font.name,
            "sha256": font.canonical_hash(),
        },
        "config_sha256": _config_hash(config, font),
        "targets": target_baselines,
        "combined_trace_sha256": hashlib.sha256(combined_trace_bytes).hexdigest(),
    }
    baseline_path = config.output_dir / "baseline.json"
    atomic_write_json(baseline_path, baseline, temp_dir=config.temp_dir)
    return PipelineReport(
        config=config,
        font=font,
        targets=tuple(target_runs),
        baseline_path=baseline_path,
        baseline=baseline,
    )


def _config_hash(config: WorkConfig, font: FontSpec) -> str:
    document = {
        "font_sha256": font.canonical_hash(),
        "glyphs": list(config.glyphs) if config.glyphs is not None else None,
        "targets": [target.as_dict() for target in config.targets],
        "tile": [config.tile_width, config.tile_height],
        "parallelism": config.parallelism,
        "samples": [config.sampling.samples_x, config.sampling.samples_y],
        "quantization_step": config.sampling.quantization_step,
        "fill_rule": config.fill_rule,
        "budget": config.budget.as_dict(),
        "atlas": config.atlas.as_dict(),
        "coverage_tolerance": config.coverage_tolerance,
    }
    return hashlib.sha256(canonical_json_bytes(document)).hexdigest()


def _run_descriptor_hash(
    config: WorkConfig,
    font: FontSpec,
    target: TargetConfig,
    glyph_specs: list[Any],
) -> str:
    document = {
        "config_sha256": _config_hash(config, font),
        "target": target.as_dict(),
        "glyphs": [glyph.name for glyph in glyph_specs],
    }
    return hashlib.sha256(canonical_json_bytes(document)).hexdigest()


def load_baseline(path: str | Path) -> dict[str, Any]:
    return json.loads(Path(path).read_text(encoding="utf-8"))

