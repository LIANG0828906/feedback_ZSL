"""Material shading, deterministic compositing, and block rendering."""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .math3d import luminance, normalize, srgb_encode
from .projection import ShadowMap, build_shadow_maps, sample_shadow_map
from .raster import (
    LayerFragments,
    PreparedTriangle,
    WallFragments,
    prepare_scene,
    rasterize_layer_sample,
    rasterize_wall_sample,
)
from .scene import Board, Layer, Rib, Scene
from .stars import StarField, generate_star_field, sample_star


@dataclass
class LayerShade:
    layer_index: int
    premultiplied: np.ndarray
    alpha: np.ndarray
    owner_kind: np.ndarray
    owner_id: np.ndarray
    transmission_map: np.ndarray
    reflection_map: np.ndarray
    board_samples: int
    rib_samples: int


@dataclass
class BlockStats:
    visible_fragments: dict[int, int] = field(default_factory=dict)
    board_samples: dict[int, int] = field(default_factory=dict)
    rib_samples: dict[int, int] = field(default_factory=dict)
    transmission_sum: dict[int, float] = field(default_factory=dict)
    reflection_sum: dict[int, float] = field(default_factory=dict)
    wall_samples: int = 0

    def merge(self, other: "BlockStats") -> None:
        for target, source in (
            (self.visible_fragments, other.visible_fragments),
            (self.board_samples, other.board_samples),
            (self.rib_samples, other.rib_samples),
        ):
            for key, value in source.items():
                target[key] = target.get(key, 0) + value
        for target, source in ((self.transmission_sum, other.transmission_sum), (self.reflection_sum, other.reflection_sum)):
            for key, value in source.items():
                target[key] = target.get(key, 0.0) + value
        self.wall_samples += other.wall_samples


@dataclass
class BlockResult:
    block_id: int
    x: int
    y: int
    width: int
    height: int
    rgba16: np.ndarray
    layer_map: np.ndarray
    stats: BlockStats
    color_summary: dict[str, object]


@dataclass
class PreparedRenderer:
    scene: Scene
    triangles: dict[int, list[PreparedTriangle]]
    stars: dict[tuple[int, int], StarField]
    shadows: dict[tuple[str, int, int, int], ShadowMap]

    @classmethod
    def build(cls, scene: Scene) -> "PreparedRenderer":
        triangles = prepare_scene(scene)
        stars: dict[tuple[int, int], StarField] = {}
        for layer in scene.layers:
            for board in layer.boards:
                stars[(layer.index, board.board_id)] = generate_star_field(board.texture_seed, board.resolution[0], board.resolution[1])
        shadows = build_shadow_maps(scene)
        return cls(scene=scene, triangles=triangles, stars=stars, shadows=shadows)


def _shadow_visibility(
    renderer: PreparedRenderer,
    receiver_kind: str,
    receiver_layer: int,
    receiver_id: int,
    uv: np.ndarray,
    shape: tuple[int, int],
    minimum: float,
) -> np.ndarray:
    visibility = np.ones((*shape, 3), dtype=np.float64)
    total_irradiance = np.zeros(3, dtype=np.float64)
    weighted = np.zeros((*shape, 3), dtype=np.float64)
    for light in renderer.scene.lights:
        total_irradiance += light.irradiance
        key = (receiver_kind, receiver_layer, receiver_id, light.light_id)
        coverage = renderer.shadows.get(key)
        if coverage is None:
            continue
        sampled = sample_shadow_map(coverage, uv[..., 0], uv[..., 1])
        weighted += sampled[:, None] * light.irradiance[None, :]
    denominator = np.maximum(total_irradiance, 1.0e-12)
    visibility = np.clip(1.0 - 0.84 * weighted / denominator[None, None, :], minimum, 1.0)
    return visibility


def _orient_normal(normal: np.ndarray, view_from_surface: np.ndarray) -> np.ndarray:
    oriented = normal.copy()
    flip = np.sum(oriented * view_from_surface, axis=2) > 0.0
    oriented[flip] *= -1.0
    return oriented


def _shade_board(
    renderer: PreparedRenderer,
    layer: Layer,
    board: Board,
    frag: LayerFragments,
    selection: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    scene = renderer.scene
    world = frag.world[selection]
    uv = frag.uv[selection]
    normal = frag.normal[selection]
    view = world - scene.camera.position[None, :]
    view_lengths = np.maximum(np.linalg.norm(view, axis=1), 1.0e-15)
    view = view / view_lengths[:, None]
    normal = _orient_normal(normal[None, :, :], view[None, :, :])[0]
    cosine = np.clip(np.abs(np.sum(normal * view, axis=1)), 0.08, 1.0)
    star = sample_star(renderer.stars[(layer.index, board.board_id)], uv[:, 0], uv[:, 1])
    star_luma = np.clip(luminance(star), 0.0, 1.0)
    optical = max(board.thickness, 0.1) / 0.1
    tau_effective = np.power(float(board.transmission), optical / cosine)
    tau_effective = np.clip(tau_effective, 0.0, 1.0)
    tau_local = np.clip(tau_effective * (1.0 - 0.72 * star_luma), 0.0, 1.0)
    shape = (int(np.count_nonzero(selection)),)
    visibility = _shadow_visibility(
        renderer,
        "star_board",
        layer.index,
        board.board_id,
        uv,
        shape,
        0.18,
    )
    direct_transmission = np.zeros_like(star)
    for light in scene.lights:
        angle = np.abs(np.sum(normal * light.direction[None, :], axis=1))
        direct_transmission += light.irradiance[None, :] * angle[:, None]
    transmitted_glow = board.tint[None, :] * (0.020 * direct_transmission * tau_local[:, None] * visibility)
    emission = star * visibility * (0.56 + 0.44 * tau_local[:, None]) + transmitted_glow
    alpha = np.clip(1.0 - tau_local + 0.08 * star_luma, 0.0, 1.0)
    premul = emission + board.tint[None, :] * (0.006 * visibility)
    return premul, alpha, tau_local


def _shade_rib(
    renderer: PreparedRenderer,
    layer: Layer,
    rib: Rib,
    frag: LayerFragments,
    selection: np.ndarray,
) -> tuple[np.ndarray, np.ndarray]:
    scene = renderer.scene
    world = frag.world[selection]
    normal = frag.normal[selection]
    to_surface = world - scene.camera.position[None, :]
    to_surface /= np.maximum(np.linalg.norm(to_surface, axis=1), 1.0e-15)[:, None]
    normal = _orient_normal(normal[None, :, :], to_surface[None, :, :])[0]
    view = -to_surface
    radiance = np.broadcast_to(rib.base_color * 0.022, (len(world), 3)).copy()
    rough_x = max(0.025, rib.roughness * max(0.07, 1.0 - 0.86 * rib.anisotropy))
    rough_y = max(0.025, rib.roughness * max(0.07, 1.0 + 0.86 * rib.anisotropy))
    tangent_reference = np.broadcast_to(np.array([0.0, 1.0, 0.0]), normal.shape)
    tangent = tangent_reference - normal * np.sum(normal * tangent_reference, axis=1)[:, None]
    tangent_lengths = np.linalg.norm(tangent, axis=1)
    fallback = tangent_lengths < 1.0e-7
    if np.any(fallback):
        tangent[fallback] = np.array([1.0, 0.0, 0.0])
        tangent[fallback] -= normal[fallback] * np.sum(normal[fallback] * tangent[fallback], axis=1)[:, None]
        tangent_lengths = np.linalg.norm(tangent, axis=1)
    tangent /= np.maximum(tangent_lengths, 1.0e-15)[:, None]
    bitangent = np.cross(normal, tangent)
    for light in scene.lights:
        toward_light = -light.direction
        n_dot_l = np.clip(np.sum(normal * toward_light[None, :], axis=1), 0.0, 1.0)
        half_vector = view + toward_light[None, :]
        half_vector /= np.maximum(np.linalg.norm(half_vector, axis=1), 1.0e-15)[:, None]
        v_dot_h = np.clip(np.sum(view * half_vector, axis=1), 0.0, 1.0)
        h_t = np.sum(half_vector * tangent, axis=1) / rough_x
        h_b = np.sum(half_vector * bitangent, axis=1) / rough_y
        lobe = np.exp(-1.35 * (h_t * h_t + h_b * h_b))
        fresnel = rib.reflectance + (1.0 - rib.reflectance) * np.power(1.0 - v_dot_h, 5.0)
        specular = rib.reflectance * lobe * (0.20 + 0.80 * fresnel) * n_dot_l
        diffuse = rib.base_color[None, :] * (0.10 * rib.reflectance) * light.irradiance[None, :] * n_dot_l[:, None]
        radiance += rib.base_color[None, :] * light.irradiance[None, :] * (specular * 1.55)[:, None] + diffuse
    reflection_energy = luminance(radiance)
    return radiance, reflection_energy


def shade_layer(renderer: PreparedRenderer, layer: Layer, frag: LayerFragments) -> LayerShade:
    shape = frag.mask.shape
    premul = np.zeros((*shape, 3), dtype=np.float64)
    alpha = np.zeros(shape, dtype=np.float64)
    owner_kind = np.zeros(shape, dtype=np.uint8)
    owner_id = np.full(shape, -1, dtype=np.int32)
    transmission_map = np.zeros(shape, dtype=np.float64)
    reflection_map = np.zeros(shape, dtype=np.float64)
    board_samples = 0
    rib_samples = 0
    for board in sorted(layer.boards, key=lambda item: item.board_id):
        selection = frag.mask & (frag.kind == 2) & (frag.object_id == board.board_id)
        count = int(np.count_nonzero(selection))
        if count == 0:
            continue
        board_premul, board_alpha, tau = _shade_board(renderer, layer, board, frag, selection)
        premul[selection] = board_premul
        alpha[selection] = board_alpha
        owner_kind[selection] = 2
        owner_id[selection] = board.board_id
        transmission_map[selection] = tau
        board_samples += count
    for rib in sorted(layer.ribs, key=lambda item: item.rib_id):
        selection = frag.mask & (frag.kind == 1) & (frag.object_id == rib.rib_id)
        count = int(np.count_nonzero(selection))
        if count == 0:
            continue
        rib_radiance, reflection_energy = _shade_rib(renderer, layer, rib, frag, selection)
        premul[selection] = rib_radiance
        alpha[selection] = 1.0
        owner_kind[selection] = 1
        owner_id[selection] = rib.rib_id
        reflection_map[selection] = reflection_energy
        rib_samples += count
    return LayerShade(
        layer_index=layer.index,
        premultiplied=premul,
        alpha=alpha,
        owner_kind=owner_kind,
        owner_id=owner_id,
        transmission_map=transmission_map,
        reflection_map=reflection_map,
        board_samples=board_samples,
        rib_samples=rib_samples,
    )


def shade_wall(renderer: PreparedRenderer, frag: WallFragments) -> tuple[np.ndarray, np.ndarray]:
    scene = renderer.scene
    wall = scene.wall
    shape = frag.mask.shape
    premul = np.zeros((*shape, 3), dtype=np.float64)
    alpha = np.zeros(shape, dtype=np.float64)
    if not np.any(frag.mask):
        return premul, alpha
    world = frag.world[frag.mask]
    uv = frag.uv[frag.mask]
    normal = np.broadcast_to(wall.normal, world.shape).copy()
    view = world - scene.camera.position[None, :]
    view /= np.maximum(np.linalg.norm(view, axis=1), 1.0e-15)[:, None]
    flip = np.sum(normal * view, axis=1) > 0.0
    normal[flip] *= -1.0
    total_irradiance = np.zeros(3, dtype=np.float64)
    weighted = np.zeros((len(world), 3), dtype=np.float64)
    direct = np.zeros_like(weighted)
    for light in scene.lights:
        total_irradiance += light.irradiance
        key = ("wall", -1, -1, light.light_id)
        shadow = renderer.shadows.get(key)
        if shadow is not None:
            coverage = sample_shadow_map(shadow, uv[:, 0], uv[:, 1])
            weighted += coverage[:, None] * light.irradiance[None, :]
        angle = np.clip(np.sum(normal * (-light.direction[None, :]), axis=1), 0.0, 1.0)
        direct += light.irradiance[None, :] * angle[:, None]
    visibility = np.clip(1.0 - 0.72 * weighted / np.maximum(total_irradiance, 1.0e-12)[None, :], wall.shadow_floor, 1.0)
    radiance = wall.emission[None, :] + wall.color[None, :] * (0.18 + 0.76 * direct * visibility)
    premul[frag.mask] = radiance
    alpha[frag.mask] = 1.0
    return premul, alpha


def _owner_code(layer_index: int | None, kind: int, object_id: int) -> int:
    if layer_index is None:
        layer_byte = 255
    else:
        layer_byte = layer_index + 1
    return (layer_byte << 24) | ((int(kind) & 0xFF) << 16) | (int(object_id + 1) & 0xFFFF)


def _codes_to_rgba8(codes: np.ndarray, counts: np.ndarray, samples: int) -> np.ndarray:
    height, width = codes.shape
    out = np.zeros((height, width, 4), dtype=np.uint8)
    out[..., 0] = (codes >> 24) & 0xFF
    out[..., 1] = (codes >> 16) & 0xFF
    out[..., 2] = codes & 0xFF
    out[..., 3] = np.rint(np.clip(counts / float(samples), 0.0, 1.0) * 255.0).astype(np.uint8)
    return out


def render_block(renderer: PreparedRenderer, block_id: int) -> BlockResult:
    scene = renderer.scene
    x0, y0, width, height = scene.renderer.block_rect(block_id)
    samples = scene.sample_offsets
    color_sum = np.zeros((height, width, 3), dtype=np.float64)
    alpha_sum = np.zeros((height, width), dtype=np.float64)
    owner_counts: dict[int, np.ndarray] = {}
    block_stats = BlockStats()

    for layer in scene.layers:
        block_stats.visible_fragments[layer.index] = 0
        block_stats.board_samples[layer.index] = 0
        block_stats.rib_samples[layer.index] = 0
        block_stats.transmission_sum[layer.index] = 0.0
        block_stats.reflection_sum[layer.index] = 0.0

    for sample_x, sample_y in samples:
        layer_shades: list[LayerShade] = []
        for layer in scene.layers:
            fragments = rasterize_layer_sample(
                renderer.triangles[layer.index],
                layer_index=layer.index,
                sample_x=sample_x,
                sample_y=sample_y,
                block_x=x0,
                block_y=y0,
                block_width=width,
                block_height=height,
            )
            shade = shade_layer(renderer, layer, fragments)
            layer_shades.append(shade)
            block_stats.visible_fragments[layer.index] += int(np.count_nonzero(fragments.mask))
            block_stats.board_samples[layer.index] += shade.board_samples
            block_stats.rib_samples[layer.index] += shade.rib_samples
            block_stats.transmission_sum[layer.index] += float(np.sum(shade.transmission_map))
            block_stats.reflection_sum[layer.index] += float(np.sum(shade.reflection_map))

        wall_fragments = rasterize_wall_sample(
            scene,
            sample_x=sample_x,
            sample_y=sample_y,
            block_x=x0,
            block_y=y0,
            width=width,
            height=height,
        )
        wall_premul, wall_alpha = shade_wall(renderer, wall_fragments)
        block_stats.wall_samples += int(np.count_nonzero(wall_fragments.mask))

        transmittance = np.ones((height, width), dtype=np.float64)
        color = np.zeros((height, width, 3), dtype=np.float64)
        best_score = np.zeros((height, width), dtype=np.float64)
        best_code = np.zeros((height, width), dtype=np.uint32)
        for shade in layer_shades:
            score = (np.max(shade.premultiplied, axis=2) + 0.15 * shade.alpha) * transmittance
            selected = shade.alpha > 0.0
            better = selected & (score > best_score + 1.0e-12)
            if np.any(better):
                codes = np.zeros((height, width), dtype=np.uint32)
                mask = shade.alpha > 0.0
                codes[mask] = (
                    (np.uint32(shade.layer_index + 1) << np.uint32(24))
                    | (shade.owner_kind[mask].astype(np.uint32) << np.uint32(16))
                    | ((shade.owner_id[mask].astype(np.uint32) + np.uint32(1)) & np.uint32(0xFFFF))
                )
                best_score[better] = score[better]
                best_code[better] = codes[better]
            color += shade.premultiplied * transmittance[..., None]
            transmittance *= np.clip(1.0 - shade.alpha, 0.0, 1.0)
        wall_score = np.max(wall_premul, axis=2) * transmittance
        wall_better = wall_fragments.mask & (wall_score > best_score + 1.0e-12)
        if np.any(wall_better):
            best_score[wall_better] = wall_score[wall_better]
            best_code[wall_better] = _owner_code(None, 3, 0)
        color += wall_premul * transmittance[..., None]
        transmittance *= np.clip(1.0 - wall_alpha, 0.0, 1.0)
        background = scene.renderer.background
        color += background[:3][None, None, :] * transmittance[..., None]
        total_alpha = 1.0 - transmittance * (1.0 - background[3])

        color_sum += color
        alpha_sum += np.clip(total_alpha, 0.0, 1.0)
        for code in np.unique(best_code):
            if int(code) == 0:
                continue
            owner_counts.setdefault(int(code), np.zeros((height, width), dtype=np.int32))
            owner_counts[int(code)] += (best_code == code).astype(np.int32)

    sample_count = len(samples)
    linear_color = color_sum / sample_count
    final_alpha = np.clip(alpha_sum / sample_count, 0.0, 1.0)
    tone_mapped = 1.0 - np.exp(-np.maximum(linear_color, 0.0) * scene.renderer.exposure)
    encoded = srgb_encode(tone_mapped)
    rgba16 = np.empty((height, width, 4), dtype=np.uint16)
    rgba16[..., :3] = np.rint(encoded * 65535.0).astype(np.uint16)
    rgba16[..., 3] = np.rint(final_alpha * 65535.0).astype(np.uint16)

    owner_code_map = np.zeros((height, width), dtype=np.uint32)
    owner_count_map = np.zeros((height, width), dtype=np.int32)
    for code in sorted(owner_counts):
        counts = owner_counts[code]
        better = counts > owner_count_map
        owner_code_map[better] = code
        owner_count_map[better] = counts[better]
    layer_map = _codes_to_rgba8(owner_code_map, owner_count_map, sample_count)
    color_summary = {
        "rgba16_mean": [round(float(value) / 65535.0, 9) for value in np.mean(rgba16, axis=(0, 1))],
        "layer_map_mean": [round(float(value) / 255.0, 9) for value in np.mean(layer_map, axis=(0, 1))],
    }
    return BlockResult(
        block_id=block_id,
        x=x0,
        y=y0,
        width=width,
        height=height,
        rgba16=rgba16,
        layer_map=layer_map,
        stats=block_stats,
        color_summary=color_summary,
    )
