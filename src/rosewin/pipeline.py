"""End-to-end chunked render pipeline with resumable snapshots."""

from __future__ import annotations

import json
from dataclasses import dataclass
from hashlib import sha256
from pathlib import Path

import numpy as np
from PIL import Image

from .model import SceneModel
from .projection import WallProjector
from .rasterizer import Rasterizer, encode_sqrt_gamma
from .scene import scene_digest
from .snapshot import SnapshotStore, chunk_summary

REPORT_SCHEMA = "cathedral-rose-report/v1"


@dataclass
class RenderArtifacts:
    output_dir: Path
    status: str
    resume_count: int


def _write_png(path: Path, array: np.ndarray, mode: str) -> None:
    image = Image.fromarray(array, mode=mode)
    image.save(path, format="PNG")


def _sha256_file(path: Path) -> str:
    return sha256(path.read_bytes()).hexdigest()


class RenderPipeline:
    def __init__(self, model: SceneModel, output_dir: str | Path):
        self.model = model
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)
        self.rasterizer = Rasterizer(model)
        self.projector = WallProjector(model)
        self.snapshot_path = self.output_dir / "render.snapshot.npz"
        self.store = SnapshotStore(self.snapshot_path, scene_digest(model.raw), self.rasterizer)

    def run(self, interrupt_after_chunks: int | None = None) -> RenderArtifacts:
        load = self.store.load()
        arrays = load.arrays
        chunk_done = arrays["chunk_done"]
        chunk_summaries = list(load.meta.get("chunk_summaries", []))
        self._arrays = arrays

        projection = self.projector.render()
        shadow8 = encode_sqrt_gamma(projection.wall_linear)

        completed = 0
        interrupted = False
        for chunk_index in range(self.rasterizer.chunk_count):
            if chunk_done[chunk_index]:
                completed += 1
                continue
            result = self.rasterizer.render_chunk(chunk_index)
            rgb8 = encode_sqrt_gamma(result.rgb)
            t8 = encode_sqrt_gamma(result.transmission)
            r8 = encode_sqrt_gamma(result.reflection)
            lead8 = np.rint(np.clip(result.lead, 0.0, 1.0) * 255.0).astype(np.uint8)
            row_start, row_end = result.row_start, result.row_end
            arrays["rgb"][row_start:row_end] = rgb8
            arrays["transmission"][row_start:row_end] = t8
            arrays["reflection"][row_start:row_end] = r8
            arrays["owner"][row_start:row_end] = result.owner
            arrays["owner_lanes"][row_start:row_end] = result.owner_lanes
            arrays["lead"][row_start:row_end] = lead8
            arrays["shadow"][:] = shadow8
            self._accumulate_facet_stats(result)
            chunk_done[chunk_index] = 1
            summary = chunk_summary(chunk_index, result, rgb8, result.owner, shadow8[row_start:row_end])
            chunk_summaries.append(summary)
            completed += 1
            self.store.save(arrays, chunk_summaries, load.resume_count)
            if interrupt_after_chunks is not None and completed >= interrupt_after_chunks:
                interrupted = True
                break

        if interrupted:
            report = self._interruption_report(arrays, chunk_summaries, load, completed, projection)
            self._write_json("recovery_report.json", report)
            return RenderArtifacts(self.output_dir, "interrupted", load.resume_count)

        self._write_outputs(arrays, shadow8)
        report = self._final_report(arrays, chunk_summaries, load, projection)
        self._write_json("run_report.json", report)
        return RenderArtifacts(self.output_dir, "complete", load.resume_count)

    def _accumulate_facet_stats(self, result) -> None:
        arrays = self._arrays
        owner = result.owner
        for facet in self.model.facets:
            mask = owner == facet.stable_id
            visible = int(mask.sum())
            if visible == 0:
                continue
            arrays["facet_visible"][facet.stable_id] += visible
            glass = mask & (result.lead < 0.5)
            arrays["facet_glass"][facet.stable_id] += int(glass.sum())
            arrays["facet_transmission_sum"][facet.stable_id] += result.transmission[glass].sum(axis=0)
            arrays["facet_reflection_sum"][facet.stable_id] += result.reflection[glass].sum(axis=0)

    def _write_outputs(self, arrays: dict[str, np.ndarray], shadow8: np.ndarray) -> None:
        _write_png(self.output_dir / "render.png", arrays["rgb"], "RGB")
        _write_png(self.output_dir / "transmission.png", arrays["transmission"], "RGB")
        _write_png(self.output_dir / "reflection.png", arrays["reflection"], "RGB")
        _write_png(self.output_dir / "shadow.png", arrays["shadow"], "RGB")
        _write_png(self.output_dir / "lead_mask.png", arrays["lead"], "L")
        owner = arrays["owner"].astype(np.int32)
        _write_png(self.output_dir / "owner.png", owner, "I")
        np.savez_compressed(self.output_dir / "owner_lanes.npz", owner_lanes=arrays["owner_lanes"])

    def _write_json(self, name: str, payload: dict) -> None:
        (self.output_dir / name).write_text(
            json.dumps(payload, indent=1, ensure_ascii=False), encoding="utf-8"
        )

    def _scene_block(self) -> dict:
        return {
            "schema": self.model.raw["schema"],
            "seed": self.model.seed,
            "scene_sha256": scene_digest(self.model.raw),
            "quadrant_ids": sorted({facet.quadrant for facet in self.model.facets}),
            "facet_count": len(self.model.facets),
        }

    def _interruption_report(self, arrays, chunk_summaries, load, completed, projection) -> dict:
        completed_ids = [f"c{i:04d}" for i, done in enumerate(arrays["chunk_done"]) if done]
        next_index = len(completed_ids)
        return {
            "schema_version": REPORT_SCHEMA,
            "status": "interrupted",
            "scene": self._scene_block(),
            "chunks": {
                "chunk_size": self.rasterizer.chunk_size,
                "chunk_count": self.rasterizer.chunk_count,
                "completed_count": int(completed),
                "completed_chunk_ids": completed_ids,
                "chunk_summaries": chunk_summaries,
            },
            "next_chunk_id": f"c{next_index:04d}",
            "recovery": self._recovery_block(load),
        }

    def _final_report(self, arrays, chunk_summaries, load, projection) -> dict:
        facets_report = self._facet_report(arrays)
        quadrants_report = self._quadrant_report(arrays)
        image_block = self._image_block(arrays)
        return {
            "schema_version": REPORT_SCHEMA,
            "status": "complete",
            "scene": self._scene_block(),
            "render": {
                "width": self.rasterizer.width,
                "height": self.rasterizer.height,
                "chunk_size": self.rasterizer.chunk_size,
                "gamma": self.model.render["gamma"],
                "primary_samples": self.rasterizer.samples,
                "render_sha256": _sha256_file(self.output_dir / "render.png"),
            },
            "chunks": {
                "chunk_size": self.rasterizer.chunk_size,
                "chunk_count": self.rasterizer.chunk_count,
                "completed_count": self.rasterizer.chunk_count,
                "completed_chunk_ids": [f"c{i:04d}" for i in range(self.rasterizer.chunk_count)],
                "chunk_summaries": chunk_summaries,
            },
            "quadrants": quadrants_report,
            "facets": facets_report,
            "projection": {
                "sample_count": projection.sample_count,
                "samples_per_pixel": self.projector.spp,
                "landing_points": projection.landing_points,
                "edge_width_samples": projection.edge_width_samples,
            },
            "image": self._image_block(arrays),
            "composition": {
                "gamma": "sqrt",
                "layers": ["background", "transmission", "reflection", "lead"],
                "facet_order": "ascending stable_id from (quadrant, facet)",
                "owner_tie_break": "lowest stable_id",
                "reflection_composite": "background + transmission + reflection, lead override then clip",
            },
            "recovery": self._recovery_block(load),
        }

    def _recovery_block(self, load) -> dict:
        return {
            "snapshot_file": self.snapshot_path.name,
            "snapshot_found": bool(load.found),
            "snapshot_used": bool(load.used),
            "snapshot_discarded": bool(load.discarded),
            "snapshot_discard_reason": load.discard_reason,
            "resume_requested": bool(load.used or load.discarded),
            "resume_count": int(load.resume_count),
        }

    def _facet_report(self, arrays) -> list[dict]:
        report = []
        for facet in self.model.facets:
            visible = int(arrays["facet_visible"][facet.stable_id])
            glass = max(int(arrays["facet_glass"][facet.stable_id]), 1)
            t_mean = arrays["facet_transmission_sum"][facet.stable_id] / glass
            r_mean = arrays["facet_reflection_sum"][facet.stable_id] / glass
            t_energy = float(t_mean.sum())
            r_energy = float(r_mean.sum())
            total = max(t_energy + r_energy, 1e-12)
            texture_table = self.rasterizer.materials[facet.stable_id].table
            report.append(
                {
                    "quadrant": facet.quadrant,
                    "facet": facet.facet,
                    "stable_id": facet.stable_id,
                    "transmittance": facet.transmittance,
                    "glaze_reflectance": facet.glaze_reflectance,
                    "lead_width": facet.lead_width,
                    "lead_depth": facet.lead_depth,
                    "visible_pixels": visible,
                    "glass_pixels": int(arrays["facet_glass"][facet.stable_id]),
                    "transmission_mean_linear": [float(v) for v in t_mean],
                    "reflection_mean_linear": [float(v) for v in r_mean],
                    "transmission_ratio": float(t_energy / total),
                    "reflection_ratio": float(r_energy / total),
                    "texture_sha256": sha256(texture_table.tobytes()).hexdigest(),
                }
            )
        return report

    def _quadrant_report(self, arrays) -> list[dict]:
        report = []
        owner = arrays["owner"]
        for quadrant in sorted({facet.quadrant for facet in self.model.facets}):
            ids = [facet.stable_id for facet in self.model.facets if facet.quadrant == quadrant]
            mask = np.isin(owner, ids)
            report.append(
                {
                    "quadrant": quadrant,
                    "visible_pixel_count": int(mask.sum()),
                    "visible_facet_count": len([sid for sid in ids if arrays["facet_visible"][sid] > 0]),
                    "visible_facet_ids": [sid for sid in ids if arrays["facet_visible"][sid] > 0],
                }
            )
        return report

    def _image_block(self, arrays: dict[str, np.ndarray]) -> dict:
        names = {
            "rgb": "render.png",
            "transmission": "transmission.png",
            "reflection": "reflection.png",
            "shadow": "shadow.png",
            "owner": "owner.png",
            "lead": "lead_mask.png",
        }
        block = {}
        for key, name in names.items():
            path = self.output_dir / name
            with Image.open(path) as image:
                shape = list(np.asarray(image).shape)
            block[key] = {"path": name, "sha256": _sha256_file(path), "shape": shape}
        lanes_path = self.output_dir / "owner_lanes.npz"
        block["owner_lanes"] = {
            "path": "owner_lanes.npz",
            "sha256": _sha256_file(lanes_path),
            "shape": list(arrays["owner_lanes"].shape),
            "order": "ascending stable_id per pixel",
        }
        return block
