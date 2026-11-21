from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

ALLOWED_DEGRADATION_STAGES = (
    "reduce_sphere_samples",
    "shrink_internal_batch",
    "reduce_preview_resolution",
)

ALLOWED_ADAPTATION = ("bradford", "cat02", "none")
ALLOWED_TARGETS = ("srgb", "display-p3")


@dataclass(frozen=True)
class GridSpec:
    start_nm: float = 380.0
    end_nm: float = 780.0
    step_nm: float = 5.0

    @property
    def sample_count(self) -> int:
        return int(round((self.end_nm - self.start_nm) / self.step_nm)) + 1


@dataclass(frozen=True)
class BakeConfig:
    parallelism: int = 4
    per_task_memory_bytes: int = 1 << 20
    global_memory_bytes: int = 4 << 20
    degradation_sequence: tuple = ALLOWED_DEGRADATION_STAGES
    grid: GridSpec = field(default_factory=GridSpec)
    observer: str = "cie1931_2deg"
    white_point: str = "D65"
    adaptation: str = "bradford"
    targets: tuple = ALLOWED_TARGETS
    sphere_samples: int = 256
    internal_batch: int = 8
    preview_resolution: int = 64
    samples_dir: str = "samples"
    snapshots_dir: str = "snapshots"
    trace_path: str = "snapshots/trace.jsonl"
    verify_order: bool = True

    @staticmethod
    def from_dict(raw: dict) -> "BakeConfig":
        grid_raw = raw.get("grid", {})
        grid = GridSpec(
            start_nm=float(grid_raw.get("start_nm", 380.0)),
            end_nm=float(grid_raw.get("end_nm", 780.0)),
            step_nm=float(grid_raw.get("step_nm", 5.0)),
        )
        seq = tuple(raw.get("degradation_sequence", ALLOWED_DEGRADATION_STAGES))
        unknown = [s for s in seq if s not in ALLOWED_DEGRADATION_STAGES]
        if unknown:
            raise ValueError(f"unknown degradation stages: {unknown}")
        adaptation = raw.get("adaptation", "bradford")
        if adaptation not in ALLOWED_ADAPTATION:
            raise ValueError(f"unknown adaptation: {adaptation}")
        targets = tuple(raw.get("targets", ALLOWED_TARGETS))
        bad_targets = [t for t in targets if t not in ALLOWED_TARGETS]
        if bad_targets:
            raise ValueError(f"unknown targets: {bad_targets}")
        cfg = BakeConfig(
            parallelism=int(raw.get("parallelism", 4)),
            per_task_memory_bytes=int(raw.get("per_task_memory_bytes", 1 << 20)),
            global_memory_bytes=int(raw.get("global_memory_bytes", 4 << 20)),
            degradation_sequence=seq,
            grid=grid,
            observer=str(raw.get("observer", "cie1931_2deg")),
            white_point=str(raw.get("white_point", "D65")),
            adaptation=adaptation,
            targets=targets,
            sphere_samples=int(raw.get("sphere_samples", 256)),
            internal_batch=int(raw.get("internal_batch", 8)),
            preview_resolution=int(raw.get("preview_resolution", 64)),
            samples_dir=str(raw.get("samples_dir", "samples")),
            snapshots_dir=str(raw.get("snapshots_dir", "snapshots")),
            trace_path=str(raw.get("trace_path", "snapshots/trace.jsonl")),
            verify_order=bool(raw.get("verify_order", True)),
        )
        if cfg.parallelism < 1:
            raise ValueError("parallelism must be >= 1")
        if cfg.per_task_memory_bytes > cfg.global_memory_bytes:
            raise ValueError("per-task quota must not exceed global quota")
        return cfg


def load_config(path) -> BakeConfig:
    raw = json.loads(Path(path).read_text(encoding="utf-8"))
    return BakeConfig.from_dict(raw)
