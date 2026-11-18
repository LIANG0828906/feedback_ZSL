"""Thin-film optics, deterministic microstructure tables, and caustic samples."""

from __future__ import annotations

import math
from dataclasses import dataclass
from functools import lru_cache
from typing import Any

import numpy as np

from .hashing import hash_u64, smooth_value_noise, unit_float
from .scene import SceneBundle

_RGB_TO_XYZ = np.array(
    [
        [3.2406, -1.5372, -0.4986],
        [-0.9689, 1.8758, 0.0415],
        [0.0557, -0.2040, 1.0570],
    ],
    dtype=np.float64,
)
_XYZ_TO_RGB = np.linalg.inv(_RGB_TO_XYZ)
_LOCAL_UP = np.array([0.0, -1.0, 0.0], dtype=np.float64)
FILM_COLOR_GAIN = 13.0


def _gaussian_term(wavelengths: np.ndarray, mean: float, left_sigma: float, right_sigma: float) -> np.ndarray:
    sigma = np.where(wavelengths < mean, left_sigma, right_sigma)
    return np.exp(-0.5 * ((wavelengths - mean) / sigma) ** 2)


@lru_cache(maxsize=8)
def _spectral_basis(start: float, end: float, step: float) -> tuple[np.ndarray, np.ndarray, float]:
    wavelengths = np.arange(start, end + step * 0.5, step, dtype=np.float64)
    x_bar = (
        1.056 * _gaussian_term(wavelengths, 599.8, 37.9, 31.0)
        + 0.362 * _gaussian_term(wavelengths, 442.0, 16.0, 26.7)
        - 0.065 * _gaussian_term(wavelengths, 501.1, 20.4, 26.2)
    )
    y_bar = (
        0.821 * _gaussian_term(wavelengths, 568.8, 46.9, 40.5)
        + 0.286 * _gaussian_term(wavelengths, 530.9, 16.3, 31.1)
    )
    z_bar = (
        1.217 * _gaussian_term(wavelengths, 437.0, 11.8, 36.0)
        + 0.681 * _gaussian_term(wavelengths, 459.0, 26.0, 13.8)
    )
    cie = np.stack([x_bar, y_bar, z_bar], axis=1) * step
    white_y = float(np.sum(y_bar) * step)
    return wavelengths, cie, white_y


def film_linear_rgb(
    thickness_nm: np.ndarray | float,
    cos_air: np.ndarray | float,
    film: dict[str, Any],
) -> np.ndarray:
    """Evaluate an Airy two-interface thin film over a CIE 1931 spectrum."""
    thickness = np.asarray(thickness_nm, dtype=np.float64)
    cosine = np.clip(np.asarray(cos_air, dtype=np.float64), 1.0e-4, 1.0)
    n = float(film["refractive_index"])
    substrate = float(film["substrate_refractive_index"])
    alpha = float(film["absorption_coefficient"])
    start, end, step = (float(item) for item in film["spectrum_nm"])
    wavelengths, cie, white_y = _spectral_basis(start, end, step)
    cos_transmitted = np.sqrt(np.maximum(1.0 - (1.0 - cosine * cosine) / (n * n), 1.0e-6))
    phase = (
        4.0
        * math.pi
        * n
        * thickness[..., None]
        * cos_transmitted[..., None]
        / wavelengths
    )
    r12 = (1.0 - n) / (1.0 + n)
    r23 = (n - substrate) / (n + substrate)
    numerator = (
        r12 * r12
        + r23 * r23
        + 2.0 * r12 * r23 * np.cos(phase)
    )
    denominator = (
        1.0
        + r12 * r12 * r23 * r23
        + 2.0 * r12 * r23 * np.cos(phase)
    )
    attenuation = np.exp(
        -alpha
        * thickness[..., None]
        / np.maximum(cos_transmitted[..., None], 0.2)
    )
    reflectance = np.maximum(numerator / denominator, 0.0) * attenuation
    xyz = reflectance @ cie
    rgb = np.maximum(xyz @ _XYZ_TO_RGB.T, 0.0)
    return rgb * (FILM_COLOR_GAIN / white_y)


def chromaticity(rgb: np.ndarray) -> np.ndarray:
    peak = np.max(rgb, axis=-1, keepdims=True)
    return np.where(peak > 1.0e-12, rgb / np.maximum(peak, 1.0e-12), 0.0)


def thickness_envelope(film: dict[str, Any]) -> tuple[float, float]:
    base = float(film["base_thickness_nm"])
    perturb = float(film["thickness_perturbation_nm"])
    gradient = float(film["thickness_gradient_nm"])
    return base - perturb - gradient, base + perturb + gradient


def spatial_thickness_offset(points: np.ndarray, film: dict[str, Any]) -> np.ndarray:
    points = np.asarray(points, dtype=np.float64)
    gradient = float(film["thickness_gradient_nm"])
    return gradient * (
        0.55 * np.sin(2.3 * points[..., 0] + 0.7)
        + 0.45 * np.cos(2.0 * points[..., 2] - 0.4)
    )


def membership_window(bary: np.ndarray) -> np.ndarray:
    bary = np.asarray(bary, dtype=np.float64)
    return np.clip(27.0 * bary[..., 0] * bary[..., 1] * bary[..., 2], 0.0, 1.0)


@dataclass
class FilmTables:
    thickness_delta: np.ndarray
    micro_normals: np.ndarray
    table_size: int

    def thickness(
        self,
        face_ids: np.ndarray,
        bary: np.ndarray,
        points: np.ndarray,
        film: dict[str, Any],
    ) -> np.ndarray:
        face_ids = np.asarray(face_ids, dtype=np.int64)
        base = float(film["base_thickness_nm"])
        field = self.thickness_delta[face_ids] * membership_window(bary)
        return base + field + spatial_thickness_offset(points, film)

    def normals(
        self,
        face_ids: np.ndarray,
        bary: np.ndarray,
    ) -> np.ndarray:
        face_ids = np.asarray(face_ids, dtype=np.int64)
        bary = np.asarray(bary, dtype=np.float64)
        total_side = bary[..., 1] + bary[..., 2]
        s = np.clip(total_side, 0.0, 1.0)
        t = np.where(total_side > 1.0e-12, bary[..., 2] / np.maximum(total_side, 1.0e-12), 0.0)
        coord = (self.table_size - 1) * s
        row = (self.table_size - 1) * t
        i0 = np.clip(np.floor(coord).astype(np.int64), 0, self.table_size - 1)
        j0 = np.clip(np.floor(row).astype(np.int64), 0, self.table_size - 1)
        i1 = np.minimum(i0 + 1, self.table_size - 1)
        j1 = np.minimum(j0 + 1, self.table_size - 1)
        fi = coord - i0
        fj = row - j0
        table = self.micro_normals[face_ids]
        n00 = table[np.arange(face_ids.shape[0]), i0, j0]
        n10 = table[np.arange(face_ids.shape[0]), i1, j0]
        n01 = table[np.arange(face_ids.shape[0]), i0, j1]
        n11 = table[np.arange(face_ids.shape[0]), i1, j1]
        a = n00 + (n10 - n00) * fi[:, None]
        b = n01 + (n11 - n01) * fi[:, None]
        normal = a + (b - a) * fj[:, None]
        return normal / np.maximum(np.linalg.norm(normal, axis=1, keepdims=True), 1.0e-12)


def _canonical_face_vertices(
    vertices: np.ndarray,
    triangle: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    order = np.argsort(triangle)
    indices = triangle[order]
    points = vertices[indices]
    normal = np.cross(points[1] - points[0], points[2] - points[0])
    if float(np.linalg.norm(normal)) <= 1.0e-18:
        return points[0], points[1], points[2]
    center = points.mean(axis=0)
    center[1] -= 0.13
    if float(np.dot(normal, center)) < 0.0:
        points = points[[0, 2, 1]]
    return points[0], points[1], points[2]


def build_film_tables(bundle: SceneBundle) -> FilmTables:
    """Build tables in face-ID order, independent of triangle traversal order."""
    film = bundle.scene["film"]
    max_face = int(bundle.face_ids.max())
    size = int(film["normal_table_size"])
    tilt = float(film["normal_tilt"])
    micro_seed = int(film["microstructure_seed"])
    thickness_seed = int(film["thickness_seed"])
    normals = np.zeros((max_face + 1, size, size, 3), dtype=np.float64)
    deltas = np.zeros((max_face + 1,), dtype=np.float64)
    perturbation = float(film["thickness_perturbation_nm"])

    for ordinal in np.argsort(bundle.face_ids, kind="stable"):
        face_id = int(bundle.face_ids[ordinal])
        face_seed = int(bundle.face_seeds[ordinal])
        p0, p1, p2 = _canonical_face_vertices(bundle.vertices, bundle.triangles[ordinal])
        base_normal = np.cross(p1 - p0, p2 - p0)
        base_normal /= max(np.linalg.norm(base_normal), 1.0e-12)
        axis = np.zeros(3, dtype=np.float64)
        axis[int(np.argmin(np.abs(base_normal)))] = 1.0
        tangent = np.cross(base_normal, axis)
        tangent /= max(np.linalg.norm(tangent), 1.0e-12)
        bitangent = np.cross(base_normal, tangent)
        for i in range(size):
            u = i / (size - 1)
            for j in range(size):
                v = j / (size - 1)
                n1 = smooth_value_noise(
                    micro_seed, face_id, face_seed, 31, u, v, lattice=4
                )
                n2 = smooth_value_noise(
                    micro_seed, face_id, face_seed, 32, u, v, lattice=4
                )
                slope_u = (n1 * 2.0 - 1.0) * tilt
                slope_v = (n2 * 2.0 - 1.0) * tilt
                sample = base_normal + slope_u * tangent + slope_v * bitangent
                normals[face_id, i, j] = sample / np.linalg.norm(sample)
        sign = unit_float(thickness_seed, face_id, face_seed, 11) * 2.0 - 1.0
        deltas[face_id] = perturbation * sign
    return FilmTables(thickness_delta=deltas, micro_normals=normals, table_size=size)


def caustic_barycentrics(density: int, seed: int, face_id: int) -> np.ndarray:
    """Return deterministic surface samples; density is samples per face edge."""
    count = density * density
    output = np.zeros((count, 3), dtype=np.float64)
    output[0] = (1.0 / 3.0, 1.0 / 3.0, 1.0 / 3.0)
    for index in range(1, count):
        u = unit_float(seed, face_id, index, 51)
        v = unit_float(seed, face_id, index, 52)
        if u + v > 1.0:
            u = 1.0 - u
            v = 1.0 - v
        output[index] = (1.0 - u - v, u, v)
    return output


def penumbra_half_width(
    distance_leaf: np.ndarray | float,
    distance_source: np.ndarray | float,
    penumbra: dict[str, Any],
) -> np.ndarray:
    """Monotone soft-shadow half-width in world units."""
    distance_leaf = np.maximum(np.asarray(distance_leaf, dtype=np.float64), 1.0e-6)
    distance_source = np.maximum(np.asarray(distance_source, dtype=np.float64), 1.0e-6)
    harmonic = distance_leaf * distance_source / (distance_leaf + distance_source)
    return float(penumbra["base_width"]) + float(penumbra["distance_gain"]) * harmonic


@dataclass
class CausticSamples:
    source_id: str
    face_ids: np.ndarray
    surface_points: np.ndarray
    thickness_nm: np.ndarray
    landing_points: np.ndarray
    landing_valid: np.ndarray
    amplitude: np.ndarray
    tint: np.ndarray
    sigma_world: np.ndarray
    distance_leaf: np.ndarray
    distance_source: np.ndarray

    @property
    def valid_indices(self) -> np.ndarray:
        return np.flatnonzero(self.landing_valid)


def _ray_occluded(
    points: np.ndarray,
    source: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
) -> np.ndarray:
    """Return True where the segment point->source crosses another triangle."""
    if points.shape[0] == 0:
        return np.zeros((0,), dtype=bool)
    vector = source[None, :] - points
    distance = np.linalg.norm(vector, axis=1)
    direction = vector / np.maximum(distance[:, None], 1.0e-12)
    v0 = vertices[triangles[:, 0]]
    edge1 = vertices[triangles[:, 1]] - v0
    edge2 = vertices[triangles[:, 2]] - v0
    pvec = np.cross(direction[:, None, :], edge2[None, :, :])
    det = np.einsum("kj,ikj->ik", edge1, pvec)
    valid_det = np.abs(det) > 1.0e-11
    inv_det = np.where(valid_det, 1.0 / np.where(valid_det, det, 1.0), 0.0)
    tvec = points[:, None, :] - v0[None, :, :]
    u = np.einsum("ikj,ikj->ik", tvec, pvec) * inv_det
    qvec = np.cross(tvec, edge1[None, :, :])
    v = np.einsum("ikj,ikj->ik", direction[:, None, :], qvec) * inv_det
    t = np.einsum("kj,ikj->ik", edge2, qvec) * inv_det
    hit = (
        valid_det
        & (u >= 1.0e-7)
        & (v >= 1.0e-7)
        & (u + v <= 1.0 - 1.0e-7)
        & (t > 1.0e-7)
        & (t < distance[:, None] * (1.0 - 1.0e-7))
    )
    return np.any(hit, axis=1)


def build_caustic_samples(
    bundle: SceneBundle,
    tables: FilmTables,
) -> dict[str, CausticSamples]:
    scene = bundle.scene
    film = scene["film"]
    leaf = scene["leaf"]
    density = int(leaf["caustic_sampling_density"])
    caustic_seed = int(hash_u64(int(scene["seed"]), 301) & 0x7FFFFFFF)
    leaf_y = float(leaf["y"])
    bounds = [float(v) for v in leaf["bounds"]]
    center = np.array([0.0, 0.13, 0.0], dtype=np.float64)
    all_results: dict[str, CausticSamples] = {}

    for light in sorted(scene["lights"], key=lambda item: item["id"]):
        source = np.asarray(light["position"], dtype=np.float64)
        intensity = float(light["intensity"])
        light_color = np.asarray(light["color"], dtype=np.float64)
        face_records: list[int] = []
        bary_records: list[np.ndarray] = []
        for ordinal, face_id in enumerate(bundle.face_ids):
            barys = caustic_barycentrics(density, caustic_seed, int(face_id))
            for bary in barys:
                face_records.append(ordinal)
                bary_records.append(bary)
        ordinals = np.asarray(face_records, dtype=np.int64)
        bary = np.asarray(bary_records, dtype=np.float64)
        fids = bundle.face_ids[ordinals]
        p0 = bundle.vertices[bundle.triangles[ordinals, 0]]
        p1 = bundle.vertices[bundle.triangles[ordinals, 1]]
        p2 = bundle.vertices[bundle.triangles[ordinals, 2]]
        points = (
            bary[:, 0, None] * p0
            + bary[:, 1, None] * p1
            + bary[:, 2, None] * p2
        )
        normals = tables.normals(fids, bary)
        thickness = tables.thickness(fids, bary, points, film)
        to_light = source[None, :] - points
        distance_source = np.linalg.norm(to_light, axis=1)
        light_dir = to_light / np.maximum(distance_source[:, None], 1.0e-12)
        cosine = np.einsum("ij,ij->i", normals, light_dir)
        occluded = _ray_occluded(points, source, bundle.vertices, bundle.triangles)

        focus = light_dir + _LOCAL_UP[None, :]
        focus /= np.maximum(np.linalg.norm(focus, axis=1, keepdims=True), 1.0e-12)
        focus_lobe = np.maximum(np.einsum("ij,ij->i", normals, focus), 0.0) ** 24
        light_rgb = film_linear_rgb(thickness, np.clip(cosine, 0.0, 1.0), film)
        tint = chromaticity(light_rgb)
        tint = tint * tint
        tint = tint * tint
        tint /= np.maximum(np.max(tint, axis=1, keepdims=True), 1.0e-12)
        reflect_luminance = np.maximum(light_rgb @ np.array([0.2126, 0.7152, 0.0722]), 0.0)
        cos_transmitted = np.sqrt(
            np.maximum(
                1.0
                - (1.0 - np.clip(cosine, 0.0, 1.0) ** 2)
                / float(film["refractive_index"]) ** 2,
                1.0e-6,
            )
        )
        absorption = np.exp(
            -float(film["absorption_coefficient"])
            * thickness
            / np.maximum(cos_transmitted, 0.2)
        )
        amplitude = (
            intensity
            * np.clip(cosine, 0.0, 1.0)
            * (0.12 + 0.88 * focus_lobe)
            * absorption
            * (0.35 + reflect_luminance)
            / (2.0 + 0.20 * distance_source * distance_source)
        )
        amplitude = np.where((cosine > 0.0) & (~occluded), amplitude, 0.0)
        amplitude = amplitude * float(
            np.dot(light_color, np.array([0.2126, 0.7152, 0.0722]))
        )
        tint = tint * light_color
        tint /= np.maximum(np.max(tint, axis=1, keepdims=True), 1.0e-12)

        delta = points - source[None, :]
        plane_t = (leaf_y - source[1]) / np.where(
            np.abs(delta[:, 1]) > 1.0e-12, delta[:, 1], -1.0e-12
        )
        landing = source[None, :] + plane_t[:, None] * delta
        landing_valid = (
            (amplitude > 1.0e-12)
            & (plane_t > 1.0)
            & (landing[:, 0] >= bounds[0])
            & (landing[:, 0] <= bounds[1])
            & (landing[:, 2] >= bounds[2])
            & (landing[:, 2] <= bounds[3])
        )
        distance_leaf = np.maximum(points[:, 1] - leaf_y, 1.0e-6)
        penumbra = leaf["penumbra"]
        sigma = penumbra_half_width(distance_leaf, distance_source, penumbra)
        minimum_pixels = float(penumbra["min_pixel_width"])
        if minimum_pixels > 0.0:
            primary_camera = scene["cameras"][0]
            sigma = np.maximum(
                sigma,
                minimum_pixels * pixel_world_size(primary_camera, leaf_y),
            )
        all_results[str(light["id"])] = CausticSamples(
            source_id=str(light["id"]),
            face_ids=fids,
            surface_points=points,
            thickness_nm=thickness,
            landing_points=landing,
            landing_valid=landing_valid,
            amplitude=amplitude,
            tint=tint,
            sigma_world=sigma,
            distance_leaf=distance_leaf,
            distance_source=distance_source,
        )
    return all_results


def caustic_point_contributions(
    samples: dict[str, CausticSamples],
    points: np.ndarray,
    gain: float,
) -> dict[str, np.ndarray]:
    """Evaluate each source independently; the caller sums canonical light IDs."""
    points = np.atleast_2d(np.asarray(points, dtype=np.float64))
    result: dict[str, np.ndarray] = {}
    for source_id in sorted(samples):
        sample = samples[source_id]
        mask = sample.landing_valid & (sample.amplitude > 0.0)
        if not np.any(mask):
            result[source_id] = np.zeros((points.shape[0], 3), dtype=np.float64)
            continue
        landing = sample.landing_points[mask]
        amplitude = sample.amplitude[mask]
        tint = sample.tint[mask]
        sigma = sample.sigma_world[mask]
        dx = points[:, None, 0] - landing[None, :, 0]
        dz = points[:, None, 2] - landing[None, :, 2]
        gaussian = np.exp(
            -0.5 * (dx * dx + dz * dz) / (sigma[None, :] * sigma[None, :])
        )
        weighted = gaussian * amplitude[None, :]
        denominator = max(float(np.sum(amplitude)), 1.0e-12)
        rgb = np.einsum("ps,sc->pc", weighted, tint) * (gain / denominator)
        result[source_id] = rgb
    return result


def pixel_world_size(camera: dict[str, Any], leaf_y: float) -> float:
    position = np.asarray(camera["position"], dtype=np.float64)
    target = np.asarray(camera["target"], dtype=np.float64)
    up = np.asarray(camera["up"], dtype=np.float64)
    forward = target - position
    forward /= np.linalg.norm(forward)
    right = np.cross(forward, up)
    right /= np.linalg.norm(right)
    true_up = np.cross(right, forward)
    if abs(float(forward[1])) > 1.0e-9:
        t = (leaf_y - position[1]) / forward[1]
    else:
        t = float(np.linalg.norm(target - position))
    center = position + t * forward
    half_height = t * math.tan(math.radians(float(camera["fov_y_deg"])) * 0.5)
    return 2.0 * half_height / int(camera["height"])
