from __future__ import annotations

import numpy as np

from dustrender.incremental import IncrementalRenderer, analyze_dependencies
from dustrender.config import RenderConfig
from dustrender.scene import Scene
from dustrender.scheduler import RenderScheduler


def _config() -> RenderConfig:
    return RenderConfig.from_dict(
        {
            "chunk_width": 6,
            "chunk_height": 6,
            "parallelism": 4,
            "task_budget": 100000,
            "base_step": 0.12,
            "max_step": 0.8,
            "min_step": 0.04,
            "shadow_samples": 2,
            "seam_validation": False,
            "compare_precision": False,
        }
    )


def _change(scene: Scene) -> Scene:
    field = scene.density_fields["main"]
    block = next(item for item in field.blocks if item.index == (0, 0, 0))
    values = np.frombuffer(
        scene.payload,
        dtype="<f4",
        count=block.count,
        offset=block.offset,
    ).reshape(block.shape).copy()
    return scene.with_density_block(
        "main", (0, 0, 0), np.clip(values * 0.9 + 0.05, 0, 1).astype("<f4")
    )


def test_density_change_invalidates_subset_and_matches_full_render(smoke_path) -> None:
    scene = Scene.load(smoke_path)
    changed = _change(scene)
    config = _config()
    renderer = IncrementalRenderer()
    base = renderer.render_initial(scene, config)
    report = renderer.render_update(base, changed, config)
    assert report.reused
    assert report.affected
    assert set(report.reused).isdisjoint(report.affected)
    assert renderer.preview.version == 2
    full = RenderScheduler().render(changed, config)
    assert np.array_equal(report.run.image, full.image)


def test_camera_change_is_conservatively_full_frame(smoke_path) -> None:
    scene = Scene.load(smoke_path)
    metadata = dict(scene.metadata)
    import copy

    metadata = copy.deepcopy(metadata)
    metadata["camera"]["world_to_camera"][0][3] += 0.05
    from conftest import pack_scene

    changed = Scene.from_bytes(pack_scene(metadata, scene.payload))
    from dustrender.chunks import build_chunk_table

    chunks = build_chunk_table(scene.width, scene.height, 6, 6)
    affected, reasons, _, conservative = analyze_dependencies(scene, changed, chunks)
    assert affected == {chunk.id for chunk in chunks}
    assert "camera" in reasons
    assert conservative
