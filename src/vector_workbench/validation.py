from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any, Mapping, Sequence

from .errors import ErrorCode, WorkbenchError
from .geometry import path_area_scene, path_bbox_scene, signed_path_area_scene
from .model import (
    CubicSegment,
    Injection,
    Scene,
    Stage,
    Style,
    VectorPath,
    canonical_json_hash,
)

DEFAULT_STYLE_CONTRACT = {
    "opacity": [0.0, 1.0],
    "stroke_width": [0.0, 10000.0],
    "blend_modes": ["normal", "add", "multiply"],
    "join_modes": ["round", "bevel", "miter"],
    "path_kinds": ["fill", "stroke"],
}

KNOWN_INJECTIONS = {
    "self_intersection",
    "degenerate",
    "clip",
    "stage_failure",
}


def _decode(message: str, **kwargs: Any) -> WorkbenchError:
    return WorkbenchError(ErrorCode.E_DECODE, message, **kwargs)


def _required(mapping: Mapping[str, Any], key: str, context: str) -> Any:
    if key not in mapping:
        raise _decode(f"missing required field {key!r} in {context}")
    return mapping[key]


def _number(value: Any, context: str, *, field: str | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _decode(f"{context} must be numeric", field=field)
    result = float(value)
    if not math.isfinite(result):
        raise _decode(f"{context} must be finite", field=field)
    return result


def _integer(value: Any, context: str, *, field: str | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise _decode(f"{context} must be an integer", field=field)
    return int(value)


def _pair_ints(value: Any, context: str) -> tuple[int, int]:
    if not isinstance(value, (list, tuple)) or len(value) != 2:
        raise _decode(f"{context} must be a two-element integer array")
    return (
        _integer(value[0], f"{context}[0]"),
        _integer(value[1], f"{context}[1]"),
    )


def _bounds(
    value: Any, context: str, *, allow_zero_extent: bool = False
) -> tuple[float, float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 4:
        raise _decode(f"{context} must contain four numbers")
    result = tuple(_number(v, f"{context}[{i}]") for i, v in enumerate(value))
    if allow_zero_extent:
        invalid = result[2] < result[0] or result[3] < result[1]
    else:
        invalid = result[2] <= result[0] or result[3] <= result[1]
    if invalid:
        raise _decode(f"{context} must have positive width and height")
    return result  # type: ignore[return-value]


def _load_json(path: str | Path, label: str) -> Any:
    source = Path(path)
    try:
        return json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise _decode(f"{label} file not found: {source}") from exc
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise _decode(f"failed to decode {label} file {source}: {exc}") from exc


def _load_truth(path: str | Path) -> dict[str, Any]:
    value = _load_json(path, "truth manifest")
    if not isinstance(value, dict) or not isinstance(value.get("scenes"), dict):
        raise _decode("truth manifest must contain a scenes object")
    return value


def _style_from_raw(
    raw: Any,
    *,
    path_id: str,
    contract: Mapping[str, Any],
) -> Style:
    if not isinstance(raw, dict):
        raise _decode("style must be an object", path_id=path_id)
    fill_raw = _required(raw, "fill_color", f"style of {path_id}")
    stroke_raw = _required(raw, "stroke_color", f"style of {path_id}")
    if not isinstance(fill_raw, (list, tuple)) or len(fill_raw) != 3:
        raise _decode("fill_color must be an RGB array", path_id=path_id)
    if not isinstance(stroke_raw, (list, tuple)) or len(stroke_raw) != 3:
        raise _decode("stroke_color must be an RGB array", path_id=path_id)
    fill = tuple(_number(v, "fill_color", field="fill_color") for v in fill_raw)
    stroke = tuple(_number(v, "stroke_color", field="stroke_color") for v in stroke_raw)
    if any(v < 0.0 or v > 1.0 for v in fill + stroke):
        raise _decode("RGB style components must be in [0, 1]", path_id=path_id)

    opacity = _number(
        _required(raw, "opacity", f"style of {path_id}"), "opacity", field="opacity"
    )
    opacity_range = contract.get("opacity", [0.0, 1.0])
    if not opacity_range[0] <= opacity <= opacity_range[1]:
        raise _decode("opacity is outside the manifest style contract", path_id=path_id)

    width = _number(
        _required(raw, "stroke_width", f"style of {path_id}"),
        "stroke_width",
        field="stroke_width",
    )
    width_range = contract.get("stroke_width", [0.0, 10000.0])
    if not width_range[0] <= width <= width_range[1]:
        raise _decode(
            "stroke_width is outside the manifest style contract", path_id=path_id
        )

    blend = _required(raw, "blend", f"style of {path_id}")
    if blend not in contract.get("blend_modes", DEFAULT_STYLE_CONTRACT["blend_modes"]):
        raise _decode(f"unsupported blend mode {blend!r}", path_id=path_id)
    join = _required(raw, "join", f"style of {path_id}")
    if join not in contract.get("join_modes", DEFAULT_STYLE_CONTRACT["join_modes"]):
        raise _decode(f"unsupported join mode {join!r}", path_id=path_id)
    return Style(
        fill_color=fill,
        stroke_color=stroke,
        stroke_width=width,
        opacity=opacity,
        blend=str(blend),
        join=str(join),
    )


def _path_from_raw(
    raw: Any,
    *,
    coord_scale: int,
    coord_bounds: tuple[int, int, int, int],
    contract: Mapping[str, Any],
) -> VectorPath:
    if not isinstance(raw, dict):
        raise _decode("each path must be an object")
    path_id = _required(raw, "id", "path")
    if not isinstance(path_id, str) or not path_id:
        raise _decode("path id must be a non-empty string")
    name = _required(raw, "name", f"path {path_id}")
    if not isinstance(name, str):
        raise _decode("path name must be a string", path_id=path_id)
    kind = _required(raw, "kind", f"path {path_id}")
    if kind not in contract.get("path_kinds", DEFAULT_STYLE_CONTRACT["path_kinds"]):
        raise _decode(f"unsupported path kind {kind!r}", path_id=path_id)
    if _required(raw, "closed", f"path {path_id}") is not True:
        raise WorkbenchError(
            ErrorCode.E_GEOM,
            "closed contour was not declared closed",
            path_id=path_id,
            segment=0,
        )

    style = _style_from_raw(
        _required(raw, "style", f"path {path_id}"), path_id=path_id, contract=contract
    )

    injection_raw = raw.get("injection")
    injection: Injection | None = None
    if injection_raw is not None:
        if not isinstance(injection_raw, dict):
            raise _decode("injection must be an object or null", path_id=path_id)
        kind_raw = _required(injection_raw, "kind", f"injection of {path_id}")
        if kind_raw not in KNOWN_INJECTIONS:
            raise _decode(f"unknown injection kind {kind_raw!r}", path_id=path_id)
        stage_raw = injection_raw.get("stage")
        stage = None
        if stage_raw is not None:
            try:
                stage = Stage(str(stage_raw))
            except ValueError as exc:
                raise _decode(f"unknown injection stage {stage_raw!r}", path_id=path_id) from exc
        if kind_raw == "stage_failure" and stage is None:
            raise _decode("stage_failure injection requires a stage", path_id=path_id)
        injection = Injection(kind=str(kind_raw), stage=stage)

    raw_segments = _required(raw, "segments", f"path {path_id}")
    if not isinstance(raw_segments, list) or not raw_segments:
        raise _decode("segments must be a non-empty array", path_id=path_id)
    segments: list[CubicSegment] = []
    for index, item in enumerate(raw_segments):
        if not isinstance(item, dict):
            raise _decode("segment must be an object", path_id=path_id, segment=index)
        try:
            segment = CubicSegment(
                start=_pair_ints(_required(item, "start", "segment"), "start"),
                control1=_pair_ints(_required(item, "c1", "segment"), "c1"),
                control2=_pair_ints(_required(item, "c2", "segment"), "c2"),
                end=_pair_ints(_required(item, "end", "segment"), "end"),
            )
        except WorkbenchError as exc:
            exc.path_id = path_id
            exc.segment = index
            raise
        all_points = (segment.start, segment.control1, segment.control2, segment.end)
        for point in all_points:
            if not (
                coord_bounds[0] <= point[0] <= coord_bounds[2]
                and coord_bounds[1] <= point[1] <= coord_bounds[3]
            ):
                raise WorkbenchError(
                    ErrorCode.E_GEOM,
                    "quantized coordinate is outside coord_bounds",
                    path_id=path_id,
                    segment=index,
                )
        segments.append(segment)

    previous_end = segments[0].start
    for index, segment in enumerate(segments):
        if segment.start != previous_end:
            raise WorkbenchError(
                ErrorCode.E_GEOM,
                "cubic segment start is not continuous with previous endpoint",
                path_id=path_id,
                segment=index,
            )
        previous_end = segment.end
    if segments[-1].end != segments[0].start:
        raise WorkbenchError(
            ErrorCode.E_GEOM,
            "closed contour endpoint does not equal its start",
            path_id=path_id,
            segment=len(segments) - 1,
        )
    return VectorPath(
        id=path_id,
        name=name,
        kind=str(kind),
        style=style,
        segments=tuple(segments),
        injection=injection,
    )


def _check_truth(
    *,
    raw: Mapping[str, Any],
    scene: Scene,
    truth_scene: Mapping[str, Any],
    style_contract: Mapping[str, Any],
) -> None:
    declared_hash = truth_scene.get("scene_sha256")
    if declared_hash != canonical_json_hash(raw):
        raise _decode("scene content hash does not match truth manifest")
    if truth_scene.get("scene_id") != scene.scene_id:
        raise _decode("truth scene_id does not match scene")
    if int(truth_scene.get("coord_scale", -1)) != scene.coord_scale:
        raise _decode("truth coord_scale does not match scene")
    truth_bbox = _bounds(truth_scene.get("scene_bbox"), "truth scene_bbox")
    if any(abs(a - b) > 1e-9 for a, b in zip(truth_bbox, scene.render_bounds)):
        raise _decode("truth scene_bbox does not match scene render_bounds")
    scale = 1.0
    expected_size = [
        max(1, int(math.ceil((truth_bbox[2] - truth_bbox[0]) * scale - 1e-9))),
        max(1, int(math.ceil((truth_bbox[3] - truth_bbox[1]) * scale - 1e-9))),
    ]
    if list(truth_scene.get("expected_size_at_scale_1", [])) != expected_size:
        raise _decode("truth expected bitmap size is inconsistent with scene_bbox")

    path_declarations = truth_scene.get("paths")
    if not isinstance(path_declarations, dict):
        raise _decode("truth paths must be an object")
    if set(path_declarations) != {path.id for path in scene.paths}:
        raise _decode("truth path identifiers do not match scene")

    default_kinds = style_contract.get("path_kinds", DEFAULT_STYLE_CONTRACT["path_kinds"])
    for path in scene.paths:
        declaration = path_declarations[path.id]
        if not isinstance(declaration, dict):
            raise _decode("truth path declaration must be an object", path_id=path.id)
        if declaration.get("kind") not in default_kinds:
            raise _decode("truth path declaration has invalid kind", path_id=path.id)
        if declaration.get("kind") != path.kind:
            raise _decode("scene path kind does not match truth declaration", path_id=path.id)

        actual_bbox = path_bbox_scene(path, scene.coord_scale)
        declared_bbox = _bounds(
            declaration.get("bbox"),
            f"truth bbox for {path.id}",
            allow_zero_extent=True,
        )
        if any(abs(a - b) > 1e-7 for a, b in zip(actual_bbox, declared_bbox)):
            raise _decode("truth path bbox does not match scene geometry", path_id=path.id)

        actual_area = path_area_scene(path, scene.coord_scale)
        declared_area = _number(
            declaration.get("area"), f"truth area for {path.id}", field="area"
        )
        if abs(actual_area - declared_area) > max(1e-7, actual_area * 1e-7):
            raise _decode("truth path area does not match scene geometry", path_id=path.id)
        declared_signed = _number(
            declaration.get("signed_area"),
            f"truth signed area for {path.id}",
            field="signed_area",
        )
        actual_signed = signed_path_area_scene(path, scene.coord_scale)
        if abs(actual_signed - declared_signed) > max(1e-7, abs(actual_signed) * 1e-7):
            raise _decode("truth path signed_area does not match scene geometry", path_id=path.id)

        declared_style = declaration.get("style")
        if not isinstance(declared_style, dict):
            raise _decode("truth style declaration must be an object", path_id=path.id)
        expected_style = {
            "fill_color": list(path.style.fill_color),
            "stroke_color": list(path.style.stroke_color),
            "stroke_width": path.style.stroke_width,
            "opacity": path.style.opacity,
            "blend": path.style.blend,
            "join": path.style.join,
        }
        if declared_style != expected_style:
            raise _decode("scene style does not match truth declaration", path_id=path.id)


def load_scene(
    scene_path: str | Path,
    truth_manifest_path: str | Path | None = None,
) -> Scene:
    raw = _load_json(scene_path, "scene")
    if not isinstance(raw, dict):
        raise _decode("scene root must be an object")
    if _integer(_required(raw, "schema_version", "scene"), "schema_version") != 1:
        raise _decode("unsupported scene schema_version")
    scene_id = _required(raw, "scene_id", "scene")
    if not isinstance(scene_id, str) or not scene_id:
        raise _decode("scene_id must be a non-empty string")
    seed = _integer(_required(raw, "seed", "scene"), "seed")
    coord_scale = _integer(_required(raw, "coord_scale", "scene"), "coord_scale")
    if coord_scale <= 0:
        raise _decode("coord_scale must be > 0", field="coord_scale")
    raw_coord_bounds = _required(raw, "coord_bounds", "scene")
    if not isinstance(raw_coord_bounds, (list, tuple)) or len(raw_coord_bounds) != 4:
        raise _decode("coord_bounds must contain four integers")
    coord_bounds = tuple(
        _integer(v, f"coord_bounds[{i}]") for i, v in enumerate(raw_coord_bounds)
    )
    if coord_bounds[2] <= coord_bounds[0] or coord_bounds[3] <= coord_bounds[1]:
        raise _decode("coord_bounds must have positive width and height")
    render_bounds = _bounds(_required(raw, "render_bounds", "scene"), "render_bounds")
    raw_paths = _required(raw, "paths", "scene")
    if not isinstance(raw_paths, list) or not raw_paths:
        raise _decode("paths must be a non-empty array")

    truth_scene: Mapping[str, Any] | None = None
    style_contract: Mapping[str, Any] = DEFAULT_STYLE_CONTRACT
    if truth_manifest_path is not None:
        manifest = _load_truth(truth_manifest_path)
        if manifest.get("schema_version") != 1:
            raise _decode("unsupported truth manifest schema_version")
        style_contract = manifest.get("style_contract") or DEFAULT_STYLE_CONTRACT
        if not isinstance(style_contract, dict):
            raise _decode("truth style_contract must be an object")
        scene_truth = manifest["scenes"].get(scene_id)
        if not isinstance(scene_truth, dict):
            raise _decode("truth manifest has no entry for scene")
        truth_scene = dict(scene_truth)
        truth_scene["_validation_tolerances"] = manifest.get(
            "validation_tolerances",
            {
                "area_relative": 0.01,
                "curve_distance_at_scale_1": 0.75,
                "min_length_monotonic_ratios": [2.0, 1.0, 0.5, 0.25, 0.125],
            },
        )

    paths: list[VectorPath] = []
    seen: set[str] = set()
    for raw_path in raw_paths:
        path = _path_from_raw(
            raw_path,
            coord_scale=coord_scale,
            coord_bounds=coord_bounds,
            contract=style_contract,
        )
        if path.id in seen:
            raise _decode("duplicate path id", path_id=path.id)
        seen.add(path.id)
        paths.append(path)

    scene = Scene(
        scene_id=scene_id,
        seed=seed,
        coord_scale=coord_scale,
        coord_bounds=coord_bounds,
        render_bounds=render_bounds,
        paths=tuple(paths),
        raw=raw,
        truth=truth_scene,
        source_path=str(Path(scene_path).resolve()),
    )
    if truth_scene is not None:
        _check_truth(
            raw=raw,
            scene=scene,
            truth_scene=truth_scene,
            style_contract=style_contract,
        )
    return scene


def load_sample_manifest(data_dir: str | Path) -> tuple[dict[str, Any], Path]:
    data = Path(data_dir)
    manifest = _load_json(data / "samples.json", "sample manifest")
    if not isinstance(manifest, dict):
        raise _decode("sample manifest root must be an object")
    return manifest, data

