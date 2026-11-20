from __future__ import annotations

from atlas_pipeline.render import encode_png_rgb, preview_rgb, scale_nearest


def test_preview_png_encoding_is_deterministic(pipeline):
    preview = pipeline.get_preview()
    rgb = preview_rgb(preview)
    first = encode_png_rgb(scale_nearest(rgb, 3))
    second = encode_png_rgb(scale_nearest(rgb, 3))
    assert first == second
    assert first.startswith(b"\x89PNG\r\n\x1a\n")
    assert first.endswith(b"IEND\xaeB`\x82")


def test_error_heatmap_changes_rendered_preview(pipeline):
    preview = pipeline.get_preview()
    normal = preview_rgb(preview, heatmap=False)
    heat = preview_rgb(preview, heatmap=True)
    assert normal.shape == heat.shape
    assert (normal != heat).any()
