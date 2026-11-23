import os
import sys

import pytest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from cadrender.pipeline import PipelineConfig, RenderPipeline
from cadrender.samplegen import generate


@pytest.fixture(scope="session")
def workdir(tmp_path_factory):
    d = tmp_path_factory.mktemp("work")
    generate(str(d))
    return str(d)


@pytest.fixture()
def cfg(tmp_path, workdir):
    return PipelineConfig(
        workdir=workdir,
        checkpoint_dir=str(tmp_path / "ckpt"),
        export_dir=str(tmp_path / "export"),
        width=128, height=128, tile_size=32, max_workers=2,
        preview_density=12, precise_density=28,
        max_inflight_pixels=80_000,
    )


@pytest.fixture()
def pipe(cfg):
    return RenderPipeline(cfg)
