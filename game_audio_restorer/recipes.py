"""Versioned restoration recipe parsing, validation and upgrade."""

from __future__ import annotations

import copy
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

from .errors import BusinessError, ErrorCode
from .utils import canonical_json, sha256_bytes

SUPPORTED_VERSIONS = (1, 2, 3)
CHANNEL_MODES = ("preserve", "mono_mix", "left", "right")


@dataclass(frozen=True)
class SpectralSubtraction:
    segment_ms: int = 32
    oversubtraction: float = 1.5
    spectral_floor: float = 0.02

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_ms": self.segment_ms,
            "oversubtraction": self.oversubtraction,
            "spectral_floor": self.spectral_floor,
        }


@dataclass(frozen=True)
class Recipe:
    version: int
    denoise_strength: float
    gain_db: float
    remove_dc: bool
    silence_threshold_dbfs: float | None
    channel_merge_mode: str
    spectral_subtraction: SpectralSubtraction
    loudness_target_lufs: float | None
    field_mapping: tuple[dict[str, Any], ...]
    lost_fields: tuple[dict[str, Any], ...]

    def semantic_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "denoise_strength": self.denoise_strength,
            "gain_db": self.gain_db,
            "remove_dc": self.remove_dc,
            "silence_threshold_dbfs": self.silence_threshold_dbfs,
            "channel_merge_mode": self.channel_merge_mode,
            "spectral_subtraction": self.spectral_subtraction.to_dict(),
            "loudness_target_lufs": self.loudness_target_lufs,
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            **self.semantic_dict(),
            "field_mapping": list(self.field_mapping),
            "lost_fields": list(self.lost_fields),
        }

    def hash(self) -> str:
        return sha256_bytes(canonical_json(self.semantic_dict()).encode("utf-8"))


def _invalid(message: str, path: str, field: str, **extra: Any) -> BusinessError:
    return BusinessError(
        ErrorCode.RECIPE_INVALID,
        message,
        {"location": {"file": path, "field": field, **extra}},
    )


def _number(
    payload: Mapping[str, Any],
    field: str,
    path: str,
    minimum: float,
    maximum: float,
    *,
    integer: bool = False,
) -> float | int:
    if field not in payload:
        raise _invalid(f"配方缺少字段 {field}", path, field)
    value = payload[field]
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _invalid(f"字段 {field} 必须是数值", path, field, value=value)
    if not math.isfinite(float(value)):
        raise _invalid(f"字段 {field} 必须是有限数值", path, field, value=value)
    if integer and int(value) != value:
        raise _invalid(f"字段 {field} 必须是整数", path, field, value=value)
    if not (minimum <= float(value) <= maximum):
        raise _invalid(
            f"字段 {field} 越界",
            path,
            field,
            value=value,
            minimum=minimum,
            maximum=maximum,
        )
    return int(value) if integer else float(value)


def _boolean(payload: Mapping[str, Any], field: str, path: str) -> bool:
    if field not in payload:
        raise _invalid(f"配方缺少字段 {field}", path, field)
    value = payload[field]
    if type(value) is not bool:
        raise _invalid(f"字段 {field} 必须是布尔值", path, field, value=value)
    return value


def _entry(
    field: str,
    value: Any,
    source: str,
    source_version: int,
    impact: str,
    expressible: tuple[int, ...],
) -> dict[str, Any]:
    return {
        "field": field,
        "value": value,
        "source": source,
        "source_version": source_version,
        "impact": impact,
        "expressible_in_versions": list(expressible),
    }


def _field_mapping(
    version: int,
    denoise: float,
    gain: float,
    remove_dc: bool,
    gate: float | None,
    mode: str,
    spectral: SpectralSubtraction,
    loudness: float | None,
) -> tuple[dict[str, Any], ...]:
    provided = "provided"
    upgraded = "upgrade_default"
    disabled = "disabled_default"
    compat = "compatibility_default"
    return (
        _entry("denoise_strength", denoise, provided, 1, "降噪强度直接控制谱减 alpha。", (1, 2, 3)),
        _entry("gain_db", gain, provided, 1, "固定增益与限幅前增益包络。", (1, 2, 3)),
        _entry(
            "remove_dc",
            remove_dc,
            provided if version >= 2 else upgraded,
            version if version >= 2 else 1,
            "旧版未表达去直流，升级为关闭以保持原有语义。"
            if version < 2
            else "按声道独立统计并减去直流。",
            (2, 3),
        ),
        _entry(
            "silence_threshold_dbfs",
            gate,
            provided if version >= 2 else disabled,
            version if version >= 2 else 1,
            "旧版无静音门限，升级为 disabled，不对低电平区间强行归零。"
            if version < 2
            else "低于门限的完整局部区间渐入归零。",
            (2, 3),
        ),
        _entry(
            "channel_merge_mode",
            mode,
            provided if version >= 3 else compat,
            version if version >= 3 else max(version, 1),
            "旧版保持输入声道布局；v3 可显式合并/选声道。"
            if version < 3
            else "在 DSP 前完成声道布局转换。",
            (3,),
        ),
        _entry(
            "spectral_subtraction.segment_ms",
            spectral.segment_ms,
            provided if version >= 3 else compat,
            version if version >= 3 else max(version, 1),
            "旧版采用兼容默认 32 ms 分段，v3 显式提供。"
            if version < 3
            else "控制 STFT 分析窗长。",
            (3,),
        ),
        _entry(
            "spectral_subtraction.oversubtraction",
            spectral.oversubtraction,
            provided if version >= 3 else compat,
            version if version >= 3 else max(version, 1),
            "旧版采用兼容默认 1.5，v3 显式提供。"
            if version < 3
            else "与 denoise_strength 一起决定谱减系数。",
            (3,),
        ),
        _entry(
            "spectral_subtraction.spectral_floor",
            spectral.spectral_floor,
            provided if version >= 3 else compat,
            version if version >= 3 else max(version, 1),
            "旧版采用兼容默认 0.02，v3 显式提供。"
            if version < 3
            else "残留谱底，避免过度削除产生音乐噪声。",
            (3,),
        ),
        _entry(
            "loudness_target_lufs",
            loudness,
            provided if version >= 3 else disabled,
            version if version >= 3 else max(version, 1),
            "旧版不进行响度归一，升级为 disabled。"
            if version < 3
            else "按 K 加权积分响度目标修正增益，最终仍受峰值限幅。",
            (3,),
        ),
    )


_LOST_IMPACT = {
    "remove_dc": "v1 无法表达；恢复按关闭处理，跨版本比对按 disabled 解释。",
    "silence_threshold_dbfs": "v1 无法表达；恢复按 disabled 处理，静音区间输出可能不同。",
    "channel_merge_mode": "v1/v2 无法表达；保持原声道布局，不能解释 v3 合并产生的差异。",
    "spectral_subtraction.segment_ms": "v1/v2 无法表达；使用兼容默认 32 ms，频谱时间分辨率不同。",
    "spectral_subtraction.oversubtraction": "v1/v2 无法表达；使用兼容默认 1.5，降噪残留可能不同。",
    "spectral_subtraction.spectral_floor": "v1/v2 无法表达；使用兼容默认 0.02，谱底与音乐噪声可能不同。",
    "loudness_target_lufs": "v1/v2 无法表达；不执行响度归一，片段响度可能不同。",
}


def _lost_fields(version: int) -> tuple[dict[str, Any], ...]:
    first_versions = {
        "remove_dc": 2,
        "silence_threshold_dbfs": 2,
        "channel_merge_mode": 3,
        "spectral_subtraction.segment_ms": 3,
        "spectral_subtraction.oversubtraction": 3,
        "spectral_subtraction.spectral_floor": 3,
        "loudness_target_lufs": 3,
    }
    return tuple(
        {
            "field": field,
            "first_version": introduced,
            "impact_scope": "跨版本恢复与共同字段比对",
            "impact": _LOST_IMPACT[field],
        }
        for field, introduced in first_versions.items()
        if introduced > version
    )


def recipe_from_mapping(payload: Mapping[str, Any], *, source: str = "<memory>") -> Recipe:
    if not isinstance(payload, Mapping):
        raise _invalid("配方根节点必须是对象", source, "$")
    if "recipe" in payload and isinstance(payload["recipe"], Mapping):
        payload = payload["recipe"]

    version = _number(payload, "version", source, 1, 3, integer=True)
    if version not in SUPPORTED_VERSIONS:
        raise _invalid("未知配方版本", source, "version", value=version)

    denoise = float(_number(payload, "denoise_strength", source, 0.0, 1.0))
    gain = float(_number(payload, "gain_db", source, -24.0, 24.0))

    if version >= 2:
        remove_dc = _boolean(payload, "remove_dc", source)
        gate = float(
            _number(payload, "silence_threshold_dbfs", source, -80.0, -10.0)
        )
    else:
        remove_dc = False
        gate = None

    if version >= 3:
        mode = payload.get("channel_merge_mode")
        if mode is None:
            raise _invalid("配方缺少字段 channel_merge_mode", source, "channel_merge_mode")
        if mode not in CHANNEL_MODES:
            raise _invalid(
                "channel_merge_mode 非法",
                source,
                "channel_merge_mode",
                value=mode,
                allowed=list(CHANNEL_MODES),
            )
        spectral_raw = payload.get("spectral_subtraction")
        if not isinstance(spectral_raw, Mapping):
            raise _invalid(
                "配方缺少对象 spectral_subtraction",
                source,
                "spectral_subtraction",
            )
        spectral = SpectralSubtraction(
            segment_ms=int(
                _number(
                    spectral_raw,
                    "segment_ms",
                    source,
                    10,
                    100,
                    integer=True,
                )
            ),
            oversubtraction=float(
                _number(
                    spectral_raw,
                    "oversubtraction",
                    source,
                    0.5,
                    8.0,
                )
            ),
            spectral_floor=float(
                _number(
                    spectral_raw,
                    "spectral_floor",
                    source,
                    0.0,
                    0.5,
                )
            ),
        )
        loudness = float(
            _number(payload, "loudness_target_lufs", source, -40.0, -8.0)
        )
    else:
        mode = "preserve"
        spectral = SpectralSubtraction()
        loudness = None

    return Recipe(
        version=int(version),
        denoise_strength=denoise,
        gain_db=gain,
        remove_dc=remove_dc,
        silence_threshold_dbfs=gate,
        channel_merge_mode=str(mode),
        spectral_subtraction=spectral,
        loudness_target_lufs=loudness,
        field_mapping=_field_mapping(
            int(version),
            denoise,
            gain,
            remove_dc,
            gate,
            str(mode),
            spectral,
            loudness,
        ),
        lost_fields=_lost_fields(int(version)),
    )


def load_recipe(path: str | Path) -> Recipe:
    source = Path(path)
    try:
        raw = source.read_text(encoding="utf-8")
    except OSError as exc:
        raise BusinessError(
            ErrorCode.RECIPE_INVALID,
            f"无法读取配方: {exc}",
            {"location": {"file": str(source), "field": "$"}},
        ) from exc
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise BusinessError(
            ErrorCode.RECIPE_INVALID,
            "配方 JSON 解析失败",
            {
                "location": {
                    "file": str(source),
                    "field": "$",
                    "line": exc.lineno,
                    "column": exc.colno,
                }
            },
        ) from exc
    return recipe_from_mapping(payload, source=str(source))


def clone_recipe(recipe: Recipe) -> Recipe:
    """Return a detached recipe copy safe for GUI editing."""

    return recipe_from_mapping(copy.deepcopy(recipe.semantic_dict()), source="<recipe-copy>")
