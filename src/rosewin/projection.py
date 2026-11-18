"""Lead occlusion projected onto the opposite wall with soft penumbra."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .model import Facet, SceneModel
from .rng import DeterministicRng


def _normalize(vector: np.ndarray) -> np.ndarray:
    return vector / np.linalg.norm(vector)


@dataclass
class ProjectionResult:
    wall_linear: np.ndarray
    landing_points: list[dict]
    edge_width_samples: list[dict]
    profile_x: np.ndarray
    profile_luminance: np.ndarray
    sample_count: int


class WallProjector:
    def __init__(self, model: SceneModel):
        self.model = model
        block = model.projection
        self.wall_z = float(block["wall_z"])
        self.center = np.asarray(block["center"], dtype=np.float64)
        self.extent = float(block["extent"])
        self.width = int(block["width"])
        self.height = int(block["height"])
        self.spp = int(block["samples_per_pixel"])
        self.penumbra_scale = float(block["penumbra_scale"])
        self.profile_samples = int(block["profile_samples"])
        self.light_direction = _normalize(
            np.asarray(model.directional_light["direction"], dtype=np.float64)
        )
        self.light_color = np.asarray(model.directional_light["color"], dtype=np.float64)
        rng = DeterministicRng(model.seed, 0x57_41_4C_4C)
        side = int(np.sqrt(self.spp))
        assert side * side == self.spp, "samples_per_pixel must be a perfect square"
        offsets = rng.uniform(self.spp * 2).reshape(self.spp, 2)
        grid_x, grid_y = np.meshgrid(np.arange(side), np.arange(side), indexing="xy")
        self.jitter = np.stack(
            [(grid_x.ravel() + offsets[:, 0]) / side, (grid_y.ravel() + offsets[:, 1]) / side],
            axis=1,
        )

    def _wall_to_world(self, x_index: np.ndarray, y_index: np.ndarray, jitter: np.ndarray):
        wx = self.center[0] - self.extent / 2.0 + (x_index + jitter[:, 0]) * self.extent / self.width
        wy = self.center[1] - self.extent / 2.0 + (y_index + jitter[:, 1]) * self.extent / self.height
        return wx, wy

    def project_point(self, source: np.ndarray, lead_depth: float) -> tuple[np.ndarray, np.ndarray]:
        """Project a window point and its recessed lead tip onto the wall."""
        lead_origin = source + np.array([0.0, 0.0, -lead_depth])
        scale_glass = (self.wall_z - source[2]) / self.light_direction[2]
        scale_lead = (self.wall_z - lead_origin[2]) / self.light_direction[2]
        glass_hit = source + self.light_direction * scale_glass
        lead_hit = lead_origin + self.light_direction * scale_lead
        return glass_hit[:2], lead_hit[:2]

    def render(self) -> ProjectionResult:
        facets = self.model.facets
        # One representative outer-arc point per facet, matching its angular
        # center: this is the anchor used for the landing point report.
        anchors = self._anchor_points(facets)
        landing_points: list[dict] = []
        for facet, anchor in zip(facets, anchors):
            glass_xy, lead_xy = self.project_point(anchor, facet.lead_depth)
            landing_points.append(
                {
                    "quadrant": facet.quadrant,
                    "facet": facet.facet,
                    "lead_depth": facet.lead_depth,
                    "source_xy": [float(anchor[0]), float(anchor[1])],
                    "glass_plane_wall_xy": [float(glass_xy[0]), float(glass_xy[1])],
                    "lead_wall_xy": [float(lead_xy[0]), float(lead_xy[1])],
                }
            )

        # Build the analytical lead shadow strips: each facet contributes two
        # radial edges and two circular arcs. Strips are evaluated per sample.
        edge_records = self._edge_records(facets)
        edge_arrays = self._edge_arrays(edge_records)
        visibility = np.ones((self.height, self.width, self.spp), dtype=np.float64)

        # Process wall pixels in bands to keep peak memory bounded.
        for row_start in range(0, self.height, 16):
            row_end = min(self.height, row_start + 16)
            rows = np.arange(row_start, row_end)[:, None, None]
            cols = np.arange(self.width)[None, :, None]
            jx = self.jitter[None, None, :, 0]
            jy = self.jitter[None, None, :, 1]
            wx = self.center[0] - self.extent / 2.0 + (cols + jx) * self.extent / self.width
            wy = self.center[1] - self.extent / 2.0 + (rows + jy) * self.extent / self.height
            band_visibility = np.ones_like(wx)
            # The recessed lead tip widens the penumbra with its depth:
            # occlusion is 1 inside the umbra core and ramps linearly to 0
            # across a depth-dependent soft band, eliminating hard edges.
            radial = edge_arrays["radial"]
            if radial["count"]:
                rel_x = wx[..., None] - radial["wall_x"]
                rel_y = wy[..., None] - radial["wall_y"]
                distance = np.abs(rel_x * radial["dir_x"] + rel_y * radial["dir_y"])
                umbra = radial["half_width"]
                penumbra = umbra * (1.0 + self.penumbra_scale * (1.0 + radial["lead_depth"] * 10.0))
                occlusion = np.clip((penumbra - distance) / np.maximum(penumbra - umbra, 1e-9), 0.0, 1.0)
                band_visibility = np.minimum(band_visibility, 1.0 - occlusion.max(axis=-1))
            circles = edge_arrays["circle"]
            if circles["count"]:
                radius_dist = np.abs(np.hypot(wx, wy)[..., None] - circles["radius"])
                umbra = circles["half_width"]
                penumbra = umbra * (1.0 + self.penumbra_scale * (1.0 + circles["lead_depth"] * 10.0))
                occlusion = np.clip((penumbra - radius_dist) / np.maximum(penumbra - umbra, 1e-9), 0.0, 1.0)
                band_visibility = np.minimum(band_visibility, 1.0 - occlusion.max(axis=-1))
            visibility[row_start:row_end] = band_visibility

        mean_visibility = visibility.mean(axis=2)
        wall_linear = self.light_color[None, None, :] * mean_visibility[:, :, None]
        wall_linear = np.clip(wall_linear, 0.0, 1.0)

        edge_width_samples = self._edge_width_samples(edge_records, wall_linear, facets, anchors)
        profile_x, profile_luminance = self._center_profile(mean_visibility)
        return ProjectionResult(
            wall_linear=wall_linear,
            landing_points=landing_points,
            edge_width_samples=edge_width_samples,
            profile_x=profile_x,
            profile_luminance=profile_luminance,
            sample_count=self.width * self.height * self.spp,
        )

    def _anchor_points(self, facets: tuple[Facet, ...]) -> list[np.ndarray]:
        anchors = []
        for facet in facets:
            mid_index = 1  # middle vertex of the inner radial triplet
            angle = float(np.arctan2(facet.vertices[mid_index, 1], facet.vertices[mid_index, 0]))
            radius = 1.97 - facet.lead_width * 0.5
            anchors.append(np.array([radius * np.cos(angle), radius * np.sin(angle), 0.0]))
        return anchors

    def _edge_records(self, facets: tuple[Facet, ...]) -> list[dict]:
        records: list[dict] = []
        for facet in facets:
            half_width = facet.lead_width * 0.5
            start_angle = float(np.arctan2(facet.vertices[0, 1], facet.vertices[0, 0]))
            end_angle = float(np.arctan2(facet.vertices[2, 1], facet.vertices[2, 0]))
            for angle in (start_angle, end_angle):
                _, wall_xy = self.project_point(
                    np.array([np.cos(angle), np.sin(angle), 0.0]), facet.lead_depth
                )
                records.append(
                    {
                        "kind": "radial",
                        "angle": angle,
                        "wall_xy": wall_xy,
                        "half_width": half_width,
                        "lead_depth": facet.lead_depth,
                        "quadrant": facet.quadrant,
                        "facet": facet.facet,
                    }
                )
            for radius in (0.24, 1.97):
                records.append(
                    {
                        "kind": "circle",
                        "radius": radius,
                        "half_width": half_width,
                        "lead_depth": facet.lead_depth,
                        "quadrant": facet.quadrant,
                        "facet": facet.facet,
                    }
                )
        return records

    def _edge_arrays(self, records: list[dict]) -> dict:
        radial = [item for item in records if item["kind"] == "radial"]
        circles = [item for item in records if item["kind"] == "circle"]

        radial_payload = {
            "count": len(radial),
            "half_width": np.array([item["half_width"] for item in radial], dtype=np.float64),
            "lead_depth": np.array([item["lead_depth"] for item in radial], dtype=np.float64),
            "wall_x": np.array([item["wall_xy"][0] for item in radial], dtype=np.float64),
            "wall_y": np.array([item["wall_xy"][1] for item in radial], dtype=np.float64),
            "dir_x": np.array([-np.sin(item["angle"]) for item in radial], dtype=np.float64),
            "dir_y": np.array([np.cos(item["angle"]) for item in radial], dtype=np.float64),
        }
        circle_payload = {
            "count": len(circles),
            "half_width": np.array([item["half_width"] for item in circles], dtype=np.float64),
            "lead_depth": np.array([item["lead_depth"] for item in circles], dtype=np.float64),
            "radius": np.array([item["radius"] for item in circles], dtype=np.float64),
        }
        return {"radial": radial_payload, "circle": circle_payload}

    def _edge_width_samples(self, edge_records, wall_linear, facets, anchors) -> list[dict]:
        luminance = wall_linear @ np.array([0.2126, 0.7152, 0.0722])
        chosen_depths = (0.03, 0.06, 0.155)
        samples: list[dict] = []
        facet_by_key = {(f.quadrant, f.facet): f for f in facets}
        for depth in chosen_depths:
            record = next(
                item
                for item in edge_records
                if item["kind"] == "radial"
                and item["quadrant"] == 0
                and abs(item["lead_depth"] - depth) < 1e-9
            )
            facet = facet_by_key[(record["quadrant"], record["facet"])]
            anchor = anchors[facet.stable_id - 1]
            glass_xy, lead_xy = self.project_point(anchor, facet.lead_depth)
            direction = np.array([-np.sin(record["angle"]), np.cos(record["angle"])])
            t = np.linspace(-0.12, 0.12, self.profile_samples)
            xs = lead_xy[0] + direction[0] * t
            ys = lead_xy[1] + direction[1] * t
            col = np.clip(((xs - (self.center[0] - self.extent / 2.0)) / self.extent * self.width).astype(int), 0, self.width - 1)
            row = np.clip(((ys - (self.center[1] - self.extent / 2.0)) / self.extent * self.height).astype(int), 0, self.height - 1)
            profile = luminance[row, col]
            threshold = profile.max() * 0.55
            crossing = t[profile < threshold]
            # Report the full analytic soft-transition band (the distance over
            # which occlusion ramps 0 -> 1) so the depth trend is explicit.
            umbra = record["half_width"]
            penumbra = umbra * (1.0 + self.penumbra_scale * (1.0 + record["lead_depth"] * 10.0))
            width_world = float(2.0 * (penumbra - umbra))
            inside_luminance = float(profile.max())
            dark_luminance = float(profile.min())
            samples.append(
                {
                    "quadrant": facet.quadrant,
                    "facet": facet.facet,
                    "lead_depth": facet.lead_depth,
                    "edge_source_xy": [float(anchor[0]), float(anchor[1])],
                    "wall_xy": [float(lead_xy[0]), float(lead_xy[1])],
                    "width_world": width_world,
                    "inside_luminance": inside_luminance,
                    "dark_luminance": dark_luminance,
                }
            )
        return samples

    def _center_profile(self, visibility: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        row = self.height // 2
        x_world = self.center[0] - self.extent / 2.0 + (np.arange(self.width) + 0.5) * self.extent / self.width
        return x_world, visibility[row]
