"""Stage specifications, parameter contracts and dependency graph."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any, Mapping

from .errors import WorkbenchError


@dataclass(frozen=True)
class ParamSpec:
    name: str
    label: str
    kind: str
    minimum: int | float
    maximum: int | float
    default: int | float
    step: int | float


@dataclass(frozen=True)
class StageSpec:
    stage_id: str
    display_name: str
    version: int
    dependencies: tuple[str, ...]
    params: tuple[ParamSpec, ...]


_DENOISE = StageSpec(
    stage_id="denoise",
    display_name="去噪",
    version=1,
    dependencies=(),
    params=(
        ParamSpec("radius", "半径", "int", 0, 5, 1, 1),
        ParamSpec("strength", "强度", "float", 0.0, 1.0, 1.0, 0.05),
    ),
)

_SHARPEN = StageSpec(
    stage_id="sharpen",
    display_name="锐化",
    version=1,
    dependencies=("denoise",),
    params=(
        ParamSpec("radius", "半径", "int", 1, 5, 1, 1),
        ParamSpec("amount", "量", "float", 0.0, 2.0, 0.8, 0.05),
        ParamSpec("threshold", "阈值", "int", 0, 255, 2, 1),
    ),
)

_LOCAL_CONTRAST = StageSpec(
    stage_id="local_contrast",
    display_name="局部对比度",
    version=1,
    dependencies=("sharpen",),
    params=(
        ParamSpec("tile_size", "分块尺寸", "int", 8, 128, 32, 8),
        ParamSpec("clip_limit", "裁剪限制", "float", 1.0, 8.0, 2.0, 0.1),
        ParamSpec("blend", "混合", "float", 0.0, 1.0, 0.75, 0.05),
    ),
)

STAGE_SPECS: dict[str, StageSpec] = {
    spec.stage_id: spec
    for spec in (_DENOISE, _SHARPEN, _LOCAL_CONTRAST)
}
STAGE_IDS: tuple[str, ...] = tuple(STAGE_SPECS)
STAGE_VERSIONS: dict[str, int] = {
    stage_id: spec.version for stage_id, spec in STAGE_SPECS.items()
}
DEFAULT_PARAMS: dict[str, dict[str, int | float]] = {
    stage_id: {param.name: param.default for param in spec.params}
    for stage_id, spec in STAGE_SPECS.items()
}


class DependencyGraph:
    """Explicit, validated stage dependency graph."""

    def __init__(self, specs: Mapping[str, StageSpec]) -> None:
        self._specs = dict(specs)
        self._order = tuple(self._specs)
        self._downstream: dict[str, tuple[str, ...]] = {}
        for stage_id, spec in self._specs.items():
            missing = [dep for dep in spec.dependencies if dep not in self._specs]
            if missing:
                raise ValueError(f"阶段 {stage_id} 引用未知依赖: {missing}")
            self._downstream[stage_id] = tuple(
                candidate
                for candidate in self._order
                if stage_id in self._all_upstream(candidate)
            )

    def _all_upstream(self, stage_id: str) -> set[str]:
        result: set[str] = set()
        stack = list(self._specs[stage_id].dependencies)
        while stack:
            current = stack.pop()
            if current in result:
                continue
            result.add(current)
            stack.extend(self._specs[current].dependencies)
        return result

    @property
    def order(self) -> tuple[str, ...]:
        return self._order

    def dependencies(self, stage_id: str) -> tuple[str, ...]:
        return self._specs[stage_id].dependencies

    def downstream(self, stage_id: str, *, include_self: bool = False) -> tuple[str, ...]:
        stages = self._downstream[stage_id]
        return (stage_id,) + stages if include_self else stages

    def affected(self, stage_id: str) -> tuple[str, ...]:
        return self.downstream(stage_id, include_self=True)

    def validate_topological_order(self) -> None:
        seen: set[str] = set()
        for stage_id in self._order:
            for dependency in self.dependencies(stage_id):
                if dependency not in seen:
                    raise ValueError(
                        f"阶段顺序非法: {stage_id} 早于依赖 {dependency}"
                    )
            seen.add(stage_id)


GRAPH = DependencyGraph(STAGE_SPECS)
GRAPH.validate_topological_order()


def _validate_value(stage: StageSpec, spec: ParamSpec, value: Any) -> int | float:
    if spec.kind == "int":
        if isinstance(value, bool) or not isinstance(value, int):
            raise WorkbenchError(
                "E_PARA",
                f"{spec.label} 必须是整数",
                stage=stage.stage_id,
                param=spec.name,
                value=repr(value),
            )
        normalized: int | float = value
    elif spec.kind == "float":
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise WorkbenchError(
                "E_PARA",
                f"{spec.label} 必须是数值",
                stage=stage.stage_id,
                param=spec.name,
                value=repr(value),
            )
        numeric = float(value)
        if not math.isfinite(numeric):
            raise WorkbenchError(
                "E_PARA",
                f"{spec.label} 必须是有限数值",
                stage=stage.stage_id,
                param=spec.name,
                value=repr(value),
            )
        normalized = round(numeric, 6)
        if normalized == 0.0:
            normalized = 0.0
    else:
        raise ValueError(f"未知参数类型: {spec.kind}")

    if normalized < spec.minimum or normalized > spec.maximum:
        raise WorkbenchError(
            "E_PARA",
            f"{spec.label} 超出范围 [{spec.minimum}, {spec.maximum}]",
            stage=stage.stage_id,
            param=spec.name,
            value=normalized,
            minimum=spec.minimum,
            maximum=spec.maximum,
        )
    return normalized


def normalize_stage_params(
    stage_id: str,
    params: Mapping[str, Any] | None,
) -> dict[str, int | float]:
    stage = STAGE_SPECS.get(stage_id)
    if stage is None:
        raise WorkbenchError("E_PARA", f"未知阶段: {stage_id}", stage=stage_id)
    source = dict(params or {})
    known = {param.name for param in stage.params}
    unknown = sorted(set(source) - known)
    if unknown:
        param = unknown[0]
        raise WorkbenchError(
            "E_PARA",
            f"未知参数: {param}",
            stage=stage_id,
            param=param,
        )
    normalized: dict[str, int | float] = {}
    for param in stage.params:
        value = source.get(param.name, param.default)
        normalized[param.name] = _validate_value(stage, param, value)
    return normalized


def normalize_pipeline_params(
    params: Mapping[str, Mapping[str, Any]] | None,
) -> dict[str, dict[str, int | float]]:
    source = params or {}
    unknown = sorted(set(source) - set(STAGE_SPECS))
    if unknown:
        raise WorkbenchError(
            "E_PARA",
            f"未知阶段: {unknown[0]}",
            stage=unknown[0],
        )
    return {
        stage_id: normalize_stage_params(stage_id, source.get(stage_id))
        for stage_id in STAGE_IDS
    }


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def params_fingerprint(params: Mapping[str, Mapping[str, Any]]) -> str:
    normalized = normalize_pipeline_params(params)
    return hashlib.sha256(canonical_json(normalized).encode("utf-8")).hexdigest()


def changed_stages(
    old: Mapping[str, Mapping[str, Any]],
    new: Mapping[str, Mapping[str, Any]],
) -> tuple[str, ...]:
    return tuple(
        stage_id
        for stage_id in STAGE_IDS
        if canonical_json(old.get(stage_id, {}))
        != canonical_json(new.get(stage_id, {}))
    )

