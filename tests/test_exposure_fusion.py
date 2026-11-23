from __future__ import annotations

import numpy as np
import pytest

from panoworkbench.blending import compose_panorama
from panoworkbench.config import BlendConfig, ExposureConfig, PipelineConfig
from panoworkbench.errors import ErrorCode, WorkbenchError
from panoworkbench.exposure import ImageExposure, apply_exposure, estimate_exposure
from panoworkbench.geometry import compute_canvas_geometry, warp_to_canvas


def _scene(dataset, config: PipelineConfig):
    homographies = {
        index: dataset.images[index].homography for index in dataset.images
    }
    sizes = {
        index: (meta.width, meta.height) for index, meta in dataset.images.items()
    }
    geometry = compute_canvas_geometry(homographies, sizes)
    to_canvas = {
        index: geometry.translation @ homography
        for index, homography in homographies.items()
    }
    exposure = estimate_exposure(
        dataset,
        to_canvas,
        (geometry.width, geometry.height),
        config.exposure,
    )
    blend = compose_panorama(
        dataset,
        to_canvas,
        geometry,
        exposure,
        config.blend,
        config.exposure,
        max_cached_images=3,
        pair_id_by_image={0: "BASE", 1: "P0", 2: "P1"},
    )
    return geometry, to_canvas, exposure, blend


def test_exposure_estimates_recover_synthetic_brightness(sample_dataset) -> None:
    config = PipelineConfig()
    _, _, exposure, _ = _scene(sample_dataset, config)
    for index in (1, 2):
        synthetic = sample_dataset.images[index].synthetic_exposure
        expected = 1.0 / synthetic["gain"]
        estimated = exposure.images[index].gain
        assert abs(estimated - expected) / expected < 0.06
        assert exposure.images[index].fit_rms < 10.0


def test_panorama_dimensions_seam_quality_and_single_source_invariance(
    sample_dataset,
) -> None:
    config = PipelineConfig()
    geometry, to_canvas, exposure, blend = _scene(sample_dataset, config)
    assert blend.panorama.shape[:2] == (geometry.height, geometry.width)
    assert blend.seam_gradient_p95 <= config.blend.seam_gradient_limit
    assert len(blend.seams) == sample_dataset.image_count - 1
    for index in sorted(sample_dataset.images):
        image = sample_dataset.read_image(index, color=True)
        corrected, _, _ = apply_exposure(
            image,
            exposure.images[index],
            config.exposure,
            strict=False,
        )
        warped, mask = warp_to_canvas(
            corrected, to_canvas[index], (geometry.width, geometry.height)
        )
        single = (blend.coverage == 1) & (blend.owner == index)
        assert np.array_equal(blend.panorama[single], warped[single])


def test_invalid_blend_parameter_is_e_para() -> None:
    with pytest.raises(WorkbenchError) as context:
        BlendConfig(pyramid_levels=0).validate()
    assert context.value.code is ErrorCode.PARA
    assert context.value.field == "blend.pyramid_levels"


def test_exposure_overflow_is_e_clip() -> None:
    image = np.full((10, 10, 3), 250, dtype=np.uint8)
    exposure = ImageExposure(gain=4.0, offset=80.0, overlap_pixels=100, fit_rms=0.0)
    with pytest.raises(WorkbenchError) as context:
        apply_exposure(image, exposure, ExposureConfig(), strict=True)
    assert context.value.code is ErrorCode.CLIP


def test_exposure_parameter_out_of_range_is_e_para() -> None:
    image = np.zeros((5, 5, 3), dtype=np.uint8)
    exposure = ImageExposure(gain=9.0, offset=0.0, overlap_pixels=100, fit_rms=0.0)
    with pytest.raises(WorkbenchError) as context:
        apply_exposure(image, exposure, ExposureConfig(), strict=False)
    assert context.value.code is ErrorCode.PARA
