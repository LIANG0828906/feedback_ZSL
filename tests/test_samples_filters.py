from __future__ import annotations

import copy
import json
from pathlib import Path

import numpy as np
import pytest

from filter_studio.errors import WorkbenchError
from filter_studio.filters import STAGE_FUNCTIONS, denoise, local_contrast, sharpen
from filter_studio.imaging import decode_image
from filter_studio.samples import generate_samples
from filter_studio.stages import (
    DEFAULT_PARAMS,
    STAGE_IDS,
    normalize_pipeline_params,
    normalize_stage_params,
)


def test_samples_are_deterministic_and_have_parseable_truth(
    sample_bundle, tmp_path
):
    sample_dir, manifest = sample_bundle
    assert {sample["mode"] for sample in manifest["samples"]} == {
        "L",
        "LA",
        "RGB",
        "RGBA",
    }
    assert len({(sample["width"], sample["height"]) for sample in manifest["samples"]}) >= 3
    for sample in manifest["samples"]:
        truth = json.loads(
            (sample_dir / sample["truth_file"]).read_text(encoding="utf-8")
        )
        assert truth["width"] == sample["width"]
        assert truth["height"] == sample["height"]
        assert truth["channels"] == sample["channels"]
        assert truth["objects"]
        assert {obj["type"] for obj in truth["objects"]} >= {"circle", "rect", "line"}
        decoded = decode_image(sample_dir / sample["file"])
        assert decoded.width == sample["width"]
        assert decoded.height == sample["height"]
        assert decoded.channels == sample["channels"]
        assert decoded.mode == sample["mode"]

    second_dir = tmp_path / "regenerated"
    second = generate_samples(second_dir)
    assert [item["sha256"] for item in second["samples"]] == [
        item["sha256"] for item in manifest["samples"]
    ]
    first_sequences = json.loads(
        (sample_dir / "params_sequences.json").read_text(encoding="utf-8")
    )
    second_sequences = json.loads(
        (second_dir / "params_sequences.json").read_text(encoding="utf-8")
    )
    assert first_sequences == second_sequences


@pytest.mark.parametrize(
    ("stage", "param", "value"),
    [
        ("denoise", "radius", 6),
        ("denoise", "strength", -0.01),
        ("sharpen", "radius", 0),
        ("sharpen", "amount", 2.01),
        ("sharpen", "threshold", 256),
        ("local_contrast", "tile_size", 7),
        ("local_contrast", "clip_limit", 8.1),
        ("local_contrast", "blend", 1.01),
    ],
)
def test_parameter_bounds_return_e_para(stage, param, value):
    params = copy.deepcopy(DEFAULT_PARAMS)
    params[stage][param] = value
    with pytest.raises(WorkbenchError) as caught:
        normalize_pipeline_params(params)
    error = caught.value
    assert error.code == "E_PARA"
    assert error.stage == stage
    assert error.param == param


def test_unknown_parameter_and_nan_return_e_para():
    with pytest.raises(WorkbenchError) as caught:
        normalize_stage_params("sharpen", {"mystery": 1})
    assert caught.value.code == "E_PARA"
    assert caught.value.stage == "sharpen"
    assert caught.value.param == "mystery"

    with pytest.raises(WorkbenchError) as caught:
        normalize_stage_params("sharpen", {"amount": float("nan")})
    assert caught.value.code == "E_PARA"
    assert caught.value.param == "amount"


def test_all_samples_preserve_shape_channels_and_dtype(
    sample_bundle, sample_by_mode, tmp_path
):
    from filter_studio.pipeline import PipelineSession

    sample_dir, _ = sample_bundle
    for mode in ("L", "LA", "RGB", "RGBA"):
        sample = sample_by_mode[mode]
        session = PipelineSession(tmp_path / f"{mode}.snapshot")
        session.set_image(sample_dir / sample["file"])
        run = session.run()
        assert run.status == "complete", run.error
        assert run.output is not None
        assert run.output.shape == session.image.array.shape
        assert run.output.dtype == np.uint8
        assert session.image.channels == sample["channels"]
        assert session.cache.get(STAGE_IDS[-1]).output.flags.writeable is False
        with pytest.raises(ValueError):
            session.cache.get(STAGE_IDS[-1]).output[0, 0] = 0
        report = session.compare_with_full()
        assert report.equal, report.to_dict()
        assert report.incremental_cache_entries <= report.full_stage_count


def test_channels_are_filtered_independently(sample_bundle, sample_by_mode):
    sample_dir, _ = sample_bundle
    sample = sample_by_mode["RGBA"]
    image = decode_image(sample_dir / sample["file"]).array
    chained = image
    for stage_id in STAGE_IDS:
        chained = STAGE_FUNCTIONS[stage_id](chained, DEFAULT_PARAMS[stage_id])

    channel_outputs = []
    for channel_index in range(image.shape[2]):
        channel = image[..., channel_index : channel_index + 1]
        for stage_id in STAGE_IDS:
            channel = STAGE_FUNCTIONS[stage_id](
                channel, DEFAULT_PARAMS[stage_id]
            )
        channel_outputs.append(channel[..., 0])
    manually_stacked = np.stack(channel_outputs, axis=2)
    assert np.array_equal(chained, manually_stacked)


def test_raw_stage_functions_do_not_mutate_input():
    source = np.arange(64 * 64 * 3, dtype=np.uint8).reshape(64, 64, 3)
    original = source.copy()
    after_denoise = denoise(source, DEFAULT_PARAMS["denoise"])
    after_sharpen = sharpen(after_denoise, DEFAULT_PARAMS["sharpen"])
    local_contrast(after_sharpen, DEFAULT_PARAMS["local_contrast"])
    assert np.array_equal(source, original)

