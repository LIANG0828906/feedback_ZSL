from __future__ import annotations

import os

from npr_pipeline.pipeline import RenderPipeline
from npr_pipeline.preview import PreviewService
from npr_pipeline.storage import Store
from npr_pipeline.ui import render_preview_image


def test_offscreen_preview_png(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    store = Store(tmp_path / "work")
    result = RenderPipeline(store).render(balanced_scene, balanced_params)
    output = tmp_path / "preview.png"
    snapshot = render_preview_image(
        PreviewService(store),
        balanced_scene,
        640,
        420,
        str(output),
    )
    assert snapshot is not None
    assert snapshot.content_hash == result.version_hash
    assert output.read_bytes().startswith(b"\x89PNG\r\n\x1a\n")
    assert output.stat().st_size > 1000
