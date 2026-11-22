from __future__ import annotations

from pathlib import Path

from spectraforge.checkpoint import CheckpointManager
from spectraforge.metadata import load_scene
from spectraforge.parameters import default_parameters
from spectraforge.pipeline import assemble_output
from spectraforge.scheduler import ProcessingJob, ResourceConfig
from spectraforge.segmentation import make_segments, segmentation_spec

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"
SCENE_PATH = SAMPLES / "scenes" / "neon_forge_424242"


def run_complete(
    workspace: Path,
    *,
    params: dict | None = None,
    workers: int = 2,
    tile: int = 32,
    scene_path: Path = SCENE_PATH,
):
    scene = load_scene(scene_path)
    segments = make_segments(
        scene.width,
        scene.height,
        scene.band_count,
        tile_width=tile,
        tile_height=tile,
    )
    config = segmentation_spec(
        scene.width,
        scene.height,
        scene.band_count,
        split_mode="tiles",
        tile_width=tile,
        tile_height=tile,
        interpolation=str((params or scene.metadata.default_parameters)["interpolation"]),
    )
    manager = CheckpointManager(workspace, scene, segments, config)
    job = ProcessingJob(
        scene,
        manager,
        resource=ResourceConfig(workers, 64),
        params=params or scene.metadata.default_parameters,
    )
    result = job.run()
    output = assemble_output(scene, manager, require_complete=True)
    return scene, manager, output, result
