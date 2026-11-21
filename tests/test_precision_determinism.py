from __future__ import annotations

import numpy as np

from volrender.pipeline import render_preview, write_preview_outputs
from volrender.render import render_frame, summary_bytes


def test_repeated_float_renders_are_byte_identical(sample_bundle):
    loaded = sample_bundle["loaded"]
    first = render_frame(loaded.scene, loaded.config, "float64")
    second = render_frame(loaded.scene, loaded.config, "float64")
    assert first.color_bytes == second.color_bytes
    assert first.transmittance_bytes == second.transmittance_bytes
    assert summary_bytes(first.summary) == summary_bytes(second.summary)


def test_repeated_fixed_renders_are_byte_identical(sample_bundle):
    loaded = sample_bundle["loaded"]
    first = render_frame(loaded.scene, loaded.config, "fixed")
    second = render_frame(loaded.scene, loaded.config, "fixed")
    assert first.color_bytes == second.color_bytes
    assert summary_bytes(first.summary) == summary_bytes(second.summary)


def test_dual_precision_error_summary_and_outputs(
    sample_bundle, tmp_path
):
    loaded = sample_bundle["loaded"]
    preview = render_preview(
        loaded.scene, loaded.config, verify_seams=True
    )
    assert set(preview.renders) == {"float64", "fixed"}
    assert preview.precision_error is not None
    error = preview.precision_error
    assert error["all_within_bound"] is True
    assert error["max_abs_error"] > 0
    assert error["max_transmittance_abs_error"] >= 0
    assert error["max_upper_bound"] > error["max_abs_error"]
    assert preview.absolute_error is not None
    assert preview.transmittance_absolute_error is not None
    assert preview.error_bound is not None
    assert np.all(preview.absolute_error <= preview.error_bound[:, :, None])
    assert np.all(
        preview.transmittance_absolute_error <= preview.error_bound
    )

    output = write_preview_outputs(tmp_path / "preview", preview)
    current = tmp_path / "preview" / "current"
    assert current.is_symlink()
    assert (current / "color_float64.bin").read_bytes() == (
        preview.renders["float64"].color_bytes
    )
    assert (current / "color_fixed.bin").read_bytes() == (
        preview.renders["fixed"].color_bytes
    )
    assert (current / "precision_transmittance_abs_error.bin").exists()
    assert (current / "preview_summary.json").read_bytes() == (
        preview.summary_raw
    )
    assert output["version_id"] == preview.version_id

    second = write_preview_outputs(tmp_path / "preview", preview)
    assert second["version_id"] == output["version_id"]
    assert (current / "preview_summary.json").read_bytes() == (
        preview.summary_raw
    )
