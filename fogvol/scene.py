"""Scene loading, schema validation, and stable content fingerprints."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .density import generate_density
from .errors import FogError


@dataclass(frozen=True)
class Volume:
    dimensions: tuple[int, int, int]
    origin: np.ndarray
    voxel_size: np.ndarray
    density_range: tuple[float, float]
    density: np.ndarray
    noise: dict[str, Any]

    @property
    def maximum(self) -> np.ndarray:
        return self.origin + np.asarray(self.dimensions) * self.voxel_size


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_deg: float
    width: int
    height: int
    aspect: float


@dataclass(frozen=True)
class Light:
    light_id: str
    kind: str
    position: np.ndarray
    direction: np.ndarray
    color: np.ndarray
    intensity: float
    inner_cone_deg: float
    outer_cone_deg: float
    range: float
    falloff: float


@dataclass(frozen=True)
class RenderSettings:
    step: int
    substeps: int
    max_samples: int
    chunk_size: tuple[int, int]
    absorption: float
    scattering: float
    phase_g: float
    light_step: float
    light_max_samples: int
    tonemap: str
    exposure: float
    gamma: float
    channel_max: float
    convergence_tolerance: float
    ambient: np.ndarray

    @property
    def extinction(self) -> float:
        return self.absorption + self.scattering


@dataclass(frozen=True)
class Scene:
    raw: dict[str, Any]
    scene_id: str
    seed: int
    volume: Volume
    camera: Camera
    lights: tuple[Light, ...]
    render: RenderSettings
    digest: str
    render_signature: str


_TONEMAPS = {"linear", "reinhard", "aces"}


def _fail(code: str, path: str, message: str) -> None:
    raise FogError(code, path, message)


def _mapping(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        _fail("INVALID_TYPE", path, "必须是 JSON 对象")
    return value


def _required(obj: dict[str, Any], key: str, path: str) -> Any:
    if key not in obj:
        _fail("MISSING_FIELD", f"{path}.{key}" if path else key, "缺少必填字段")
    return obj[key]


def _finite_number(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        _fail("INVALID_NUMBER", path, "必须是有限数值")
    number = float(value)
    if not math.isfinite(number):
        _fail("INVALID_NUMBER", path, "必须是有限数值")
    return number


def _integer(value: Any, path: str, *, minimum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        _fail("INVALID_INTEGER", path, "必须是整数")
    if minimum is not None and value < minimum:
        _fail("INVALID_INTEGER", path, f"必须大于等于 {minimum}")
    return value


def _vec3(value: Any, path: str) -> np.ndarray:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        _fail("INVALID_VECTOR3", path, "必须是长度为 3 的数组")
    result = np.asarray(
        [_finite_number(item, f"{path}[{index}]") for index, item in enumerate(value)],
        dtype=np.float64,
    )
    return result


def _relative_error(actual: float, expected: float) -> float:
    return abs(actual - expected) / max(abs(expected), 1.0e-12)


def _parse_volume(raw: dict[str, Any], seed: int) -> Volume:
    volume_raw = _mapping(_required(raw, "volume", ""), "volume")
    dimensions_raw = _required(volume_raw, "dimensions", "volume")
    if not isinstance(dimensions_raw, list) or len(dimensions_raw) != 3:
        _fail("INVALID_DIMENSIONS", "volume.dimensions", "必须是 3 个正整数")
    dimensions = tuple(
        _integer(value, f"volume.dimensions[{index}]", minimum=1)
        for index, value in enumerate(dimensions_raw)
    )
    origin = _vec3(_required(volume_raw, "origin", "volume"), "volume.origin")
    voxel_size = _vec3(
        _required(volume_raw, "voxel_size", "volume"), "volume.voxel_size"
    )
    if np.any(voxel_size <= 0.0):
        _fail("INVALID_VOXEL_SIZE", "volume.voxel_size", "每个体素尺寸必须大于 0")

    range_raw = _required(volume_raw, "density_range", "volume")
    if not isinstance(range_raw, list) or len(range_raw) != 2:
        _fail(
            "INVALID_DENSITY_RANGE",
            "volume.density_range",
            "必须是 [最小值, 最大值]",
        )
    density_min = _finite_number(range_raw[0], "volume.density_range[0]")
    density_max = _finite_number(range_raw[1], "volume.density_range[1]")
    if density_min < 0.0 or density_max < density_min:
        _fail(
            "INVALID_DENSITY_RANGE",
            "volume.density_range",
            "必须满足 0 <= 最小值 <= 最大值",
        )

    noise = _mapping(volume_raw.get("noise", {}), "volume.noise")
    for key in ("octaves",):
        if key in noise:
            noise[key] = _integer(noise[key], f"volume.noise.{key}", minimum=1)
    for key in (
        "base_frequency",
        "lacunarity",
        "gain",
        "floor_thickness",
        "top_thickness",
        "strata_frequency",
    ):
        if key in noise:
            number = _finite_number(noise[key], f"volume.noise.{key}")
            if number <= 0.0:
                _fail(
                    "INVALID_NOISE_PARAMETER",
                    f"volume.noise.{key}",
                    "必须大于 0",
                )
            noise[key] = number

    voxel_count = int(np.prod(dimensions))
    if "density" in volume_raw:
        density_value = volume_raw["density"]
        try:
            density = np.asarray(density_value, dtype=np.float64)
        except (TypeError, ValueError):
            _fail(
                "DENSITY_NOT_NUMERIC",
                "volume.density",
                "体素数组必须只包含数值",
            )
        if density.size != voxel_count:
            _fail(
                "DENSITY_LENGTH_MISMATCH",
                "volume.density",
                f"体素数量 {density.size} 与维度乘积 {voxel_count} 不一致",
            )
        if density.shape == (voxel_count,):
            density = density.reshape(dimensions)
        elif density.shape != dimensions:
            _fail(
                "DENSITY_SHAPE_MISMATCH",
                "volume.density",
                f"数组形状 {list(density.shape)} 与维度 {list(dimensions)} 不一致",
            )
        if not np.all(np.isfinite(density)):
            _fail(
                "NONFINITE_DENSITY",
                "volume.density",
                "体素数组包含 NaN 或 Inf",
            )
        if np.any(density < density_min) or np.any(density > density_max):
            _fail(
                "DENSITY_OUT_OF_RANGE",
                "volume.density",
                f"体素值必须位于 [{density_min}, {density_max}]",
            )
        density = np.ascontiguousarray(density, dtype=np.float64)
    else:
        density = generate_density(
            seed=seed,
            dimensions=dimensions,
            origin=origin,
            voxel_size=voxel_size,
            density_range=(density_min, density_max),
            noise=noise,
        )
        if not np.all(np.isfinite(density)):
            _fail("NONFINITE_DENSITY", "volume.density", "生成的密度包含非有限值")
        if np.any(density < density_min) or np.any(density > density_max):
            _fail(
                "DENSITY_OUT_OF_RANGE",
                "volume.density",
                "生成的密度超出声明量程",
            )

    return Volume(
        dimensions=dimensions,
        origin=origin,
        voxel_size=voxel_size,
        density_range=(density_min, density_max),
        density=density,
        noise=dict(noise),
    )


def _parse_camera(raw: dict[str, Any]) -> Camera:
    camera_raw = _mapping(_required(raw, "camera", ""), "camera")
    position = _vec3(
        _required(camera_raw, "position", "camera"), "camera.position"
    )
    target = _vec3(_required(camera_raw, "target", "camera"), "camera.target")
    up = _vec3(_required(camera_raw, "up", "camera"), "camera.up")
    fov_deg = _finite_number(
        _required(camera_raw, "fov_deg", "camera"), "camera.fov_deg"
    )
    if not 0.0 < fov_deg < 180.0:
        _fail("INVALID_FOV", "camera.fov_deg", "视场角必须位于 (0, 180) 度")
    width = _integer(
        _required(camera_raw, "width", "camera"), "camera.width", minimum=1
    )
    height = _integer(
        _required(camera_raw, "height", "camera"), "camera.height", minimum=1
    )
    aspect = _finite_number(
        _required(camera_raw, "aspect", "camera"), "camera.aspect"
    )
    if aspect <= 0.0:
        _fail("INVALID_ASPECT", "camera.aspect", "宽高比必须大于 0")
    expected_aspect = width / height
    if _relative_error(aspect, expected_aspect) > 1.0e-5:
        _fail(
            "ASPECT_MISMATCH",
            "camera.aspect",
            f"宽高比 {aspect} 与 width/height {expected_aspect} 不一致",
        )
    forward = target - position
    if np.linalg.norm(forward) <= 1.0e-12:
        _fail("INVALID_CAMERA_BASIS", "camera.target", "相机视线方向不能为零")
    if np.linalg.norm(up) <= 1.0e-12:
        _fail("INVALID_CAMERA_BASIS", "camera.up", "相机 up 向量不能为零")
    if np.linalg.norm(np.cross(forward, up)) <= 1.0e-12:
        _fail(
            "INVALID_CAMERA_BASIS",
            "camera.up",
            "相机 up 向量不能与视线平行",
        )
    return Camera(
        position=position,
        target=target,
        up=up,
        fov_deg=fov_deg,
        width=width,
        height=height,
        aspect=aspect,
    )


def _parse_lights(raw: dict[str, Any], volume: Volume) -> tuple[Light, ...]:
    lights_raw = _required(raw, "lights", "")
    if not isinstance(lights_raw, list) or not lights_raw:
        _fail("INVALID_LIGHTS", "lights", "必须是非空数组")
    maximum = volume.maximum
    seen_ids: set[str] = set()
    lights: list[Light] = []
    for index, item in enumerate(lights_raw):
        path = f"lights[{index}]"
        light_raw = _mapping(item, path)
        light_id = _required(light_raw, "id", path)
        if not isinstance(light_id, str) or not light_id:
            _fail("INVALID_LIGHT_ID", f"{path}.id", "必须是非空字符串")
        if light_id in seen_ids:
            _fail("DUPLICATE_LIGHT_ID", f"{path}.id", "灯 ID 必须唯一")
        seen_ids.add(light_id)
        kind = light_raw.get("kind", "stage")
        if not isinstance(kind, str):
            _fail("INVALID_LIGHT_KIND", f"{path}.kind", "必须是字符串")
        position = _vec3(
            _required(light_raw, "position", path), f"{path}.position"
        )
        if np.all(position > volume.origin) and np.all(position < maximum):
            _fail(
                "LIGHT_INSIDE_VOLUME",
                f"{path}.position",
                "舞台灯位置不得落在雾体内部",
            )
        direction = _vec3(
            _required(light_raw, "direction", path), f"{path}.direction"
        )
        direction_norm = float(np.linalg.norm(direction))
        if direction_norm <= 1.0e-12:
            _fail("INVALID_LIGHT_DIRECTION", f"{path}.direction", "方向向量不能为零")
        direction = direction / direction_norm
        color = _vec3(_required(light_raw, "color", path), f"{path}.color")
        if np.any(color < 0.0) or np.any(color > 1.0):
            _fail("INVALID_LIGHT_COLOR", f"{path}.color", "颜色通道必须位于 [0, 1]")
        intensity = _finite_number(
            _required(light_raw, "intensity", path), f"{path}.intensity"
        )
        if intensity < 0.0:
            _fail("INVALID_LIGHT_INTENSITY", f"{path}.intensity", "强度不能小于 0")
        inner = _finite_number(
            light_raw.get("inner_cone_deg", 0.0), f"{path}.inner_cone_deg"
        )
        outer = _finite_number(
            light_raw.get("outer_cone_deg", 30.0), f"{path}.outer_cone_deg"
        )
        if not 0.0 <= inner <= outer <= 90.0 or outer <= 0.0:
            _fail(
                "INVALID_LIGHT_CONE",
                f"{path}.outer_cone_deg",
                "须满足 0 <= inner_cone_deg <= outer_cone_deg <= 90",
            )
        range_value = _finite_number(
            light_raw.get("range", 1.0e9), f"{path}.range"
        )
        if range_value <= 0.0:
            _fail("INVALID_LIGHT_RANGE", f"{path}.range", "照射范围必须大于 0")
        falloff = _finite_number(
            light_raw.get("falloff", 1.0), f"{path}.falloff"
        )
        if falloff <= 0.0:
            _fail("INVALID_LIGHT_FALLOFF", f"{path}.falloff", "衰减系数必须大于 0")
        lights.append(
            Light(
                light_id=light_id,
                kind=kind,
                position=position,
                direction=direction,
                color=color,
                intensity=intensity,
                inner_cone_deg=inner,
                outer_cone_deg=outer,
                range=range_value,
                falloff=falloff,
            )
        )
    return tuple(lights)


def _parse_render(raw: dict[str, Any]) -> RenderSettings:
    render_raw = _mapping(_required(raw, "render", ""), "render")
    step = _integer(_required(render_raw, "step", "render"), "render.step", minimum=1)
    substeps = _integer(
        render_raw.get("substeps", 4), "render.substeps", minimum=1
    )
    max_samples = _integer(
        _required(render_raw, "max_samples", "render"),
        "render.max_samples",
        minimum=1,
    )
    chunk_raw = _required(render_raw, "chunk_size", "render")
    if not isinstance(chunk_raw, list) or len(chunk_raw) != 2:
        _fail(
            "INVALID_CHUNK_SIZE",
            "render.chunk_size",
            "必须是 [块宽, 块高] 两个正整数",
        )
    chunk_size = (
        _integer(chunk_raw[0], "render.chunk_size[0]", minimum=1),
        _integer(chunk_raw[1], "render.chunk_size[1]", minimum=1),
    )
    absorption = _finite_number(
        _required(render_raw, "absorption", "render"), "render.absorption"
    )
    scattering = _finite_number(
        _required(render_raw, "scattering", "render"), "render.scattering"
    )
    if absorption < 0.0 or scattering < 0.0 or absorption + scattering <= 0.0:
        _fail(
            "INVALID_COEFFICIENTS",
            "render.absorption",
            "吸收与散射系数须非负且不能同时为零",
        )
    phase_g = _finite_number(render_raw.get("phase_g", 0.0), "render.phase_g")
    if not -0.99 <= phase_g <= 0.99:
        _fail("INVALID_PHASE_G", "render.phase_g", "相位参数必须位于 [-0.99, 0.99]")
    light_step = _finite_number(
        render_raw.get("light_step", 1.0), "render.light_step"
    )
    if light_step <= 0.0:
        _fail("INVALID_LIGHT_STEP", "render.light_step", "灯光路径步长必须大于 0")
    light_max_samples = _integer(
        render_raw.get("light_max_samples", 64),
        "render.light_max_samples",
        minimum=1,
    )
    tonemap = render_raw.get("tonemap", "aces")
    if not isinstance(tonemap, str) or tonemap not in _TONEMAPS:
        _fail(
            "INVALID_TONEMAP",
            "render.tonemap",
            f"必须是 {sorted(_TONEMAPS)} 之一",
        )
    exposure = _finite_number(render_raw.get("exposure", 1.0), "render.exposure")
    if exposure <= 0.0:
        _fail("INVALID_EXPOSURE", "render.exposure", "曝光必须大于 0")
    gamma = _finite_number(render_raw.get("gamma", 2.2), "render.gamma")
    if gamma <= 0.0:
        _fail("INVALID_GAMMA", "render.gamma", "gamma 必须大于 0")
    channel_max = _finite_number(
        render_raw.get("channel_max", 1.0), "render.channel_max"
    )
    if not 0.0 < channel_max <= 1.0:
        _fail(
            "INVALID_CHANNEL_MAX",
            "render.channel_max",
            "通道上限必须位于 (0, 1]",
        )
    tolerance = _finite_number(
        render_raw.get("convergence_tolerance", 0.02),
        "render.convergence_tolerance",
    )
    if tolerance < 0.0:
        _fail(
            "INVALID_TOLERANCE",
            "render.convergence_tolerance",
            "收敛容差不能小于 0",
        )
    ambient = _vec3(render_raw.get("ambient", [0.0, 0.0, 0.0]), "render.ambient")
    if np.any(ambient < 0.0):
        _fail("INVALID_AMBIENT", "render.ambient", "环境光通道不能小于 0")

    return RenderSettings(
        step=step,
        substeps=substeps,
        max_samples=max_samples,
        chunk_size=chunk_size,
        absorption=absorption,
        scattering=scattering,
        phase_g=phase_g,
        light_step=light_step,
        light_max_samples=light_max_samples,
        tonemap=tonemap,
        exposure=exposure,
        gamma=gamma,
        channel_max=channel_max,
        convergence_tolerance=tolerance,
        ambient=ambient,
    )


def _canonical_json(value: Any) -> bytes:
    try:
        text = json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        )
    except (TypeError, ValueError) as exc:
        raise FogError("INVALID_JSON_VALUE", "$", f"JSON 无法规范化: {exc}") from exc
    return text.encode("utf-8")


def _digest(scene_raw: dict[str, Any], density: np.ndarray) -> str:
    digest = hashlib.sha256()
    digest.update(_canonical_json(scene_raw))
    digest.update(b"\x00FOGVOL_DENSITY\x00")
    digest.update(np.ascontiguousarray(density, dtype="<f8").tobytes())
    return digest.hexdigest()


def _render_signature(
    camera: Camera, volume: Volume, render: RenderSettings
) -> str:
    payload = {
        "camera": {
            "position": camera.position.tolist(),
            "target": camera.target.tolist(),
            "up": camera.up.tolist(),
            "fov_deg": camera.fov_deg,
            "width": camera.width,
            "height": camera.height,
            "aspect": camera.aspect,
        },
        "volume": {
            "dimensions": list(volume.dimensions),
            "origin": volume.origin.tolist(),
            "voxel_size": volume.voxel_size.tolist(),
        },
        "render": {
            "step": render.step,
            "substeps": render.substeps,
            "max_samples": render.max_samples,
            "chunk_size": list(render.chunk_size),
            "absorption": render.absorption,
            "scattering": render.scattering,
            "phase_g": render.phase_g,
            "light_step": render.light_step,
            "light_max_samples": render.light_max_samples,
            "tonemap": render.tonemap,
            "exposure": render.exposure,
            "gamma": render.gamma,
            "channel_max": render.channel_max,
            "ambient": render.ambient.tolist(),
        },
    }
    return hashlib.sha256(_canonical_json(payload)).hexdigest()


def parse_scene(raw: dict[str, Any]) -> Scene:
    if not isinstance(raw, dict):
        _fail("INVALID_SCENE", "$", "场景根节点必须是 JSON 对象")
    schema_version = _required(raw, "schema_version", "")
    if schema_version != 1:
        _fail("UNSUPPORTED_SCHEMA", "schema_version", "仅支持 schema_version = 1")
    scene_id = _required(raw, "scene_id", "")
    if not isinstance(scene_id, str) or not scene_id:
        _fail("INVALID_SCENE_ID", "scene_id", "必须是非空字符串")
    seed = _integer(_required(raw, "seed", ""), "seed", minimum=0)

    volume = _parse_volume(raw, seed)
    camera = _parse_camera(raw)
    lights = _parse_lights(raw, volume)
    render = _parse_render(raw)
    digest = _digest(raw, volume.density)
    signature = _render_signature(camera, volume, render)
    return Scene(
        raw=raw,
        scene_id=scene_id,
        seed=seed,
        volume=volume,
        camera=camera,
        lights=lights,
        render=render,
        digest=digest,
        render_signature=signature,
    )


def load_scene(path: str | Path) -> Scene:
    scene_path = Path(path)
    try:
        with scene_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except OSError as exc:
        raise FogError("SCENE_IO", "$", f"无法读取场景文件: {exc.strerror}") from exc
    except json.JSONDecodeError as exc:
        raise FogError(
            "INVALID_JSON",
            "$",
            f"JSON 解析失败于第 {exc.lineno} 行第 {exc.colno} 列",
        ) from exc
    return parse_scene(raw)
