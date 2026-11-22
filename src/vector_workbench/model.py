from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Mapping


class Stage(str, Enum):
    FLATTEN = "flatten"
    COVERAGE = "coverage"
    COMPOSE = "compose"

    @classmethod
    def ordered(cls) -> tuple["Stage", ...]:
        return (cls.FLATTEN, cls.COVERAGE, cls.COMPOSE)


class CellStatus(str, Enum):
    PENDING = "pending"
    PROCESSING = "processing"
    COMMITTED = "committed"
    FAILED = "failed"


@dataclass(frozen=True)
class CubicSegment:
    start: tuple[int, int]
    control1: tuple[int, int]
    control2: tuple[int, int]
    end: tuple[int, int]


@dataclass(frozen=True)
class Style:
    fill_color: tuple[float, float, float]
    stroke_color: tuple[float, float, float]
    stroke_width: float
    opacity: float
    blend: str = "normal"
    join: str = "round"


@dataclass(frozen=True)
class Injection:
    kind: str
    stage: Stage | None = None


@dataclass(frozen=True)
class VectorPath:
    id: str
    name: str
    kind: str
    style: Style
    segments: tuple[CubicSegment, ...]
    injection: Injection | None = None


@dataclass
class Scene:
    scene_id: str
    seed: int
    coord_scale: int
    coord_bounds: tuple[int, int, int, int]
    render_bounds: tuple[float, float, float, float]
    paths: tuple[VectorPath, ...]
    raw: Mapping[str, Any]
    truth: Mapping[str, Any] | None = None
    source_path: str | None = None

    def canonical_hash(self) -> str:
        return canonical_json_hash(self.raw)

    def path_map(self) -> dict[str, VectorPath]:
        return {path.id: path for path in self.paths}


@dataclass(frozen=True)
class RenderParams:
    scale: float = 1.0
    tolerance: float = 0.25
    subsamples: int = 4
    max_parallelism: int = 2
    coverage_buffer_limit: int = 32 * 1024 * 1024
    max_stroke_pixels: float = 4096.0

    def validate(self) -> None:
        from .errors import ErrorCode, WorkbenchError

        if (
            isinstance(self.scale, bool)
            or not isinstance(self.scale, (int, float))
            or not math.isfinite(float(self.scale))
        ):
            raise WorkbenchError(ErrorCode.E_PARA, "scale must be finite", field="scale")
        if self.scale <= 0:
            raise WorkbenchError(ErrorCode.E_PARA, "scale must be > 0", field="scale")
        if (
            isinstance(self.tolerance, bool)
            or not isinstance(self.tolerance, (int, float))
            or not math.isfinite(float(self.tolerance))
        ):
            raise WorkbenchError(ErrorCode.E_PARA, "tolerance must be finite", field="tolerance")
        if self.tolerance <= 0:
            raise WorkbenchError(
                ErrorCode.E_PARA, "tolerance must be > 0", field="tolerance"
            )
        if isinstance(self.subsamples, bool) or not isinstance(self.subsamples, int):
            raise WorkbenchError(
                ErrorCode.E_PARA, "subsamples must be an integer", field="subsamples"
            )
        if not 1 <= self.subsamples <= 16:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "subsamples must be in [1, 16]",
                field="subsamples",
            )
        if isinstance(self.max_parallelism, bool) or not isinstance(
            self.max_parallelism, int
        ):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_parallelism must be an integer",
                field="max_parallelism",
            )
        if not 1 <= self.max_parallelism <= 16:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_parallelism must be in [1, 16]",
                field="max_parallelism",
            )
        if (
            isinstance(self.coverage_buffer_limit, bool)
            or not isinstance(self.coverage_buffer_limit, int)
            or self.coverage_buffer_limit <= 0
        ):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "coverage_buffer_limit must be a positive integer",
                field="coverage_buffer_limit",
            )
        if (
            isinstance(self.max_stroke_pixels, bool)
            or not isinstance(self.max_stroke_pixels, (int, float))
            or not math.isfinite(float(self.max_stroke_pixels))
            or self.max_stroke_pixels <= 0
        ):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_stroke_pixels must be finite and > 0",
                field="max_stroke_pixels",
            )

    def version_dict(self) -> dict[str, Any]:
        return {
            "scale": format(float(self.scale), ".17g"),
            "tolerance": format(float(self.tolerance), ".17g"),
            "subsamples": self.subsamples,
            "coverage_buffer_limit": self.coverage_buffer_limit,
            "max_stroke_pixels": format(float(self.max_stroke_pixels), ".17g"),
        }

    def fingerprint(self) -> str:
        payload = json.dumps(
            self.version_dict(), sort_keys=True, separators=(",", ":"), ensure_ascii=True
        ).encode("utf-8")
        return hashlib.sha256(payload).hexdigest()


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def canonical_json_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


@dataclass
class RunResult:
    status: str
    recoverable: bool
    image: Any
    progress_committed: int
    progress_total: int
    commit_sequence: list[tuple[str, str]] = field(default_factory=list)
    errors: list[dict[str, Any]] = field(default_factory=list)
    invalidated_snapshot: bool = False
    invalidation_reason: str | None = None
    diff: Any = None

    @property
    def completed(self) -> bool:
        return self.status == "completed"

