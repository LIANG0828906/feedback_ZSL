"""多版本 ICC 风格配置文件解析。

支持三个本地版本，字段命名、TRC 表达、白点可选性、意图枚举均不同：

版本 1 (legacy):  蛇形标签、colorant 列标签组装矩阵、参数 gamma TRC、
                  ICC 数字意图枚举、白点 media_white_point 可选。
版本 2 (middle):  短标签、行主序 matrix3x3、表格 TRC、短字符串意图、
                  白点 wp 可选、可选 3D LUT。
版本 3 (3.0):     驼峰/kebab 标签、matrix.rows、参数 sRGB/gamma 或表格 TRC、
                  kebab 意图、白点 whitePoint 可选。

未知版本号/必填缺失 -> E_VER；标签值越界 -> E_PARA（带源标签名）。
"""

from __future__ import annotations

import json

from .errors import CwbError, E_PARA, E_VER
from .model import (
    ColorProfile,
    INTENT_ABSOLUTE,
    INTENT_PERCEPTUAL,
    INTENT_RELATIVE,
    LUT3D,
    TRC,
)

_INTENT_V1 = {0: INTENT_PERCEPTUAL, 1: INTENT_RELATIVE, 3: INTENT_ABSOLUTE}
_INTENT_V2 = {
    "perc": INTENT_PERCEPTUAL,
    "rel": INTENT_RELATIVE,
    "abs": INTENT_ABSOLUTE,
}
_INTENT_V3 = {
    "perceptual": INTENT_PERCEPTUAL,
    "relative-colorimetric": INTENT_RELATIVE,
    "absolute-colorimetric": INTENT_ABSOLUTE,
}

_GAMMA_RANGE = (0.2, 5.0)
_MATRIX_RANGE = 4.0
_WP_RANGE = 4.0


def load_profile(path: str) -> ColorProfile:
    with open(path, "r", encoding="utf-8") as fh:
        raw = json.load(fh)
    return parse_profile(raw)


def _require(raw: dict, key: str, version: str) -> object:
    if key not in raw or raw[key] is None:
        raise CwbError(E_VER,
                       f"配置版本 {version} 缺少必填标签 '{key}'", tag=key)
    return raw[key]


def _check_finite_vec(value, tag: str, length: int,
                      bound: float | None = None) -> list[float]:
    if not isinstance(value, (list, tuple)) or len(value) != length:
        raise CwbError(E_PARA, f"标签 '{tag}' 必须是长度 {length} 的数组",
                       tag=tag)
    out: list[float] = []
    for item in value:
        if isinstance(item, bool) or not isinstance(item, (int, float)):
            raise CwbError(E_PARA, f"标签 '{tag}' 含非数值元素", tag=tag)
        num = float(item)
        if num != num or num in (float("inf"), float("-inf")):
            raise CwbError(E_PARA, f"标签 '{tag}' 含非有限数值", tag=tag)
        if bound is not None and abs(num) > bound:
            raise CwbError(E_PARA,
                           f"标签 '{tag}' 数值 {num} 越界 (|x|>{bound})",
                           tag=tag)
        out.append(num)
    return out


def _parse_intent(value, mapping: dict, tag: str) -> str:
    if value not in mapping:
        raise CwbError(E_PARA, f"标签 '{tag}' 的意图枚举 {value!r} 不受支持",
                       tag=tag)
    return mapping[value]


def _parse_param_trc(raw: dict, tag: str) -> TRC:
    kind = str(raw.get("type", raw.get("curve", "gamma"))).lower()
    if kind in ("srgb", "srgb"):
        return TRC(kind="parametric", curve="srgb")
    if kind != "gamma":
        raise CwbError(E_PARA, f"标签 '{tag}' 曲线类型 {kind!r} 非法", tag=tag)
    gamma_key = "value" if "value" in raw else "gamma"
    gamma = raw.get(gamma_key)
    if isinstance(gamma, bool) or not isinstance(gamma, (int, float)):
        raise CwbError(E_PARA, f"标签 '{tag}' 缺少数值 gamma", tag=tag)
    lo, hi = _GAMMA_RANGE
    if not lo <= float(gamma) <= hi:
        raise CwbError(E_PARA,
                       f"标签 '{tag}' gamma={gamma} 越界 [{lo},{hi}]",
                       tag=tag)
    return TRC(kind="parametric", curve="gamma", gamma=float(gamma))


def _parse_table_trc(samples, tag: str) -> TRC:
    if not isinstance(samples, (list, tuple)) or len(samples) < 2:
        raise CwbError(E_PARA,
                       f"标签 '{tag}' 表格采样点至少需要 2 个", tag=tag)
    vals = _check_finite_vec(samples, tag, len(samples), bound=1.0)
    if any(v < 0.0 or v > 1.0 for v in vals):
        raise CwbError(E_PARA, f"标签 '{tag}' 采样值必须落在 [0,1]", tag=tag)
    if vals[0] != 0.0 or vals[-1] != 1.0:
        raise CwbError(E_PARA,
                       f"标签 '{tag}' 首点须为 0、末点须为 1", tag=tag)
    if any(b < a for a, b in zip(vals, vals[1:])):
        raise CwbError(E_PARA, f"标签 '{tag}' 采样表必须单调非减", tag=tag)
    return TRC(kind="table", table=tuple(vals))


def _parse_lut(raw: dict, version: str) -> LUT3D | None:
    if not raw:
        return None
    size = raw.get("grid_size", raw.get("size"))
    if not isinstance(size, int) or not 2 <= size <= 16:
        raise CwbError(E_PARA, "3D LUT 网格规模须为 2..16 的整数",
                       tag="lut3d")
    data = raw.get("data") or raw.get("grid")
    expect = size ** 3 * 3
    flat = data if data and isinstance(data[0], (int, float)) else [
        v for triple in (data or []) for v in triple]
    if len(flat) != expect:
        raise CwbError(E_PARA,
                       f"3D LUT 数据长度 {len(flat)} != 期望 {expect}",
                       tag="lut3d")
    nums = _check_finite_vec(flat, "lut3d", expect, bound=1.0)
    grid = tuple((nums[i], nums[i + 1], nums[i + 2])
                 for i in range(0, expect, 3))
    return LUT3D(size=size, grid=grid)


def _wp(raw: dict, key: str, default_key: str | None,
        label_map: dict[str, str]) -> tuple[tuple[float, float, float] | None,
                                            tuple[float, float, float] | None]:
    wp = None
    if key in raw and raw[key] is not None:
        wp = tuple(_check_finite_vec(raw[key], key, 3, bound=_WP_RANGE))
        label_map[key] = "white_point"
    declared = None
    if default_key and raw.get(default_key) is not None:
        declared = tuple(_check_finite_vec(raw[default_key], default_key, 3,
                                           bound=_WP_RANGE))
        label_map[default_key] = "declared_default_white_point"
    return wp, declared


def parse_profile(raw: dict) -> ColorProfile:
    if not isinstance(raw, dict):
        raise CwbError(E_VER, "配置文件根节点必须是对象")
    version = _detect_version(raw)
    label_map: dict[str, str] = {}

    if version == "1":
        return _parse_v1(raw, label_map)
    if version == "2":
        return _parse_v2(raw, label_map)
    if version == "3.0":
        return _parse_v3(raw, label_map)
    raise CwbError(E_VER, f"未知配置版本号: {version!r}")


def _detect_version(raw: dict) -> str:
    if "profile_version" in raw:
        return str(raw["profile_version"])
    if "ver" in raw:
        return str(raw["ver"])
    if "schemaVersion" in raw:
        return str(raw["schemaVersion"])
    raise CwbError(E_VER, "找不到任何受支持的版本标签")


def _parse_v1(raw: dict, label_map: dict[str, str]) -> ColorProfile:
    pid = str(_require(raw, "profile_id", "1"))
    # colorant 三列组装 3x3（列主序输入，转成行主序存储）
    r = _check_finite_vec(_require(raw, "red_colorant", "1"),
                          "red_colorant", 3, _MATRIX_RANGE)
    g = _check_finite_vec(_require(raw, "green_colorant", "1"),
                          "green_colorant", 3, _MATRIX_RANGE)
    b = _check_finite_vec(_require(raw, "blue_colorant", "1"),
                          "blue_colorant", 3, _MATRIX_RANGE)
    label_map.update(red_colorant="matrix", green_colorant="matrix",
                     blue_colorant="matrix")
    matrix = (r[0], g[0], b[0], r[1], g[1], b[1], r[2], g[2], b[2])

    trc_raw = _require(raw, "red_trc", "1")
    label_map["red_trc"] = "trc"
    trc = _parse_param_trc(trc_raw if isinstance(trc_raw, dict)
                           else {"type": "gamma", "value": trc_raw},
                           "red_trc")

    wp, declared = _wp(raw, "media_white_point", "default_white_point",
                       label_map)
    intent = INTENT_RELATIVE
    if "default_rendering_intent" in raw:
        intent = _parse_intent(raw["default_rendering_intent"], _INTENT_V1,
                               "default_rendering_intent")
        label_map["default_rendering_intent"] = "default_intent"
    cs = str(raw.get("color_space", "RGB")).lower()
    return ColorProfile(
        source_version="1", profile_id=pid, color_space=cs, trc=trc,
        matrix=matrix, lut3d=None, white_point=wp,
        declared_default_white_point=declared, default_intent=intent,
        intent_map=dict(_INTENT_V1), label_map=label_map)


def _parse_v2(raw: dict, label_map: dict[str, str]) -> ColorProfile:
    pid = str(_require(raw, "id", "2"))
    matrix = tuple(_check_finite_vec(_require(raw, "matrix3x3", "2"),
                                     "matrix3x3", 9, _MATRIX_RANGE))
    label_map["matrix3x3"] = "matrix"
    trc_raw = _require(raw, "trc", "2")
    label_map["trc"] = "trc"
    if not isinstance(trc_raw, dict) or trc_raw.get("mode") != "table":
        raise CwbError(E_VER, "版本 2 要求 trc.mode == table", tag="trc")
    trc = _parse_table_trc(trc_raw["samples"], "trc.samples")
    wp, declared = _wp(raw, "wp", "declared_default_wp", label_map)
    intent = INTENT_RELATIVE
    if "intent" in raw:
        intent = _parse_intent(raw["intent"], _INTENT_V2, "intent")
        label_map["intent"] = "default_intent"
    lut3d = _parse_lut(raw.get("lut3d"), "2")
    cs = str(raw.get("cs", "rgb")).lower()
    return ColorProfile(
        source_version="2", profile_id=pid, color_space=cs, trc=trc,
        matrix=matrix, lut3d=lut3d, white_point=wp,
        declared_default_white_point=declared, default_intent=intent,
        intent_map=dict(_INTENT_V2), label_map=label_map)


def _parse_v3(raw: dict, label_map: dict[str, str]) -> ColorProfile:
    pid = str(_require(raw, "profileId", "3.0"))
    rows_raw = _require(raw, "matrix", "3.0")
    if not isinstance(rows_raw, dict) or "rows" not in rows_raw:
        raise CwbError(E_VER, "版本 3 要求 matrix.rows", tag="matrix")
    rows = rows_raw["rows"]
    if not isinstance(rows, list) or len(rows) != 3:
        raise CwbError(E_PARA, "matrix.rows 必须是 3 行", tag="matrix.rows")
    matrix = tuple(v for row in rows
                   for v in _check_finite_vec(row, "matrix.rows", 3,
                                              _MATRIX_RANGE))
    label_map["matrix"] = "matrix"
    trc_raw = _require(raw, "toneResponse", "3.0")
    label_map["toneResponse"] = "trc"
    if not isinstance(trc_raw, dict):
        raise CwbError(E_VER, "toneResponse 必须是对象", tag="toneResponse")
    kind = trc_raw.get("kind")
    if kind == "table":
        trc = _parse_table_trc(trc_raw.get("samples", []),
                               "toneResponse.samples")
    elif kind == "parametric":
        trc = _parse_param_trc(trc_raw, "toneResponse")
    else:
        raise CwbError(E_PARA, f"toneResponse.kind={kind!r} 非法",
                       tag="toneResponse.kind")
    wp, declared = _wp(raw, "whitePoint", "declaredDefaultWhitePoint",
                       label_map)
    intent = INTENT_RELATIVE
    if "renderingIntent" in raw:
        intent = _parse_intent(raw["renderingIntent"], _INTENT_V3,
                               "renderingIntent")
        label_map["renderingIntent"] = "default_intent"
    lut3d = _parse_lut(raw.get("lut3d"), "3.0")
    cs = str(raw.get("colorSpace", "rgb")).lower()
    return ColorProfile(
        source_version="3.0", profile_id=pid, color_space=cs, trc=trc,
        matrix=matrix, lut3d=lut3d, white_point=wp,
        declared_default_white_point=declared, default_intent=intent,
        intent_map=dict(_INTENT_V3), label_map=label_map)
