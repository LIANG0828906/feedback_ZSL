from __future__ import annotations

import copy

import pytest

from transparent_pipeline.errors import E_DECODE, E_GEOM, E_PARA, E_SCHEMA, PipelineError
from transparent_pipeline.renderer import RenderConfig
from transparent_pipeline.scene import Scene, load_scene, validate_scene_dict


def code_of(callable_) -> str:
    with pytest.raises(PipelineError) as caught:
        callable_()
    return caught.value.code


def test_generated_sample_contract(sample_bundle):
    scene = sample_bundle["scene"]
    truth = sample_bundle["truth"]
    assert scene.band_count == 3
    assert len(scene.media) == 3
    assert len(scene.lights) == 3
    assert len(scene.camera["trajectory"]) == 5
    assert truth["scene_hash"] == scene.content_hash
    assert len(truth["boundary_exit"]) == 18
    assert len(truth["ray_transmittance"]) == 18
    assert len(truth["ray_scattering"]) == 54
    assert {case["time"] for case in truth["boundary_exit"]} == {
        scene.camera["trajectory"][0]["time"],
        scene.camera["trajectory"][-1]["time"],
    }


def test_schema_missing_field(sample_bundle):
    data = copy.deepcopy(sample_bundle["scene_dict"])
    del data["media"][0]["ior"]
    with pytest.raises(PipelineError) as caught:
        validate_scene_dict(data)
    assert caught.value.code == E_SCHEMA
    assert "media[0].ior" in caught.value.field


def test_decode_bad_json(tmp_path):
    path = tmp_path / "broken.json"
    path.write_text("{not-json", encoding="utf-8")
    with pytest.raises(PipelineError) as caught:
        load_scene(path)
    assert caught.value.code == E_DECODE


def test_geometry_open_box(sample_bundle):
    data = copy.deepcopy(sample_bundle["scene_dict"])
    data["media"][1]["boundary"]["max"] = data["media"][1]["boundary"]["min"]
    with pytest.raises(PipelineError) as caught:
        validate_scene_dict(data)
    assert caught.value.code == E_GEOM
    assert "media[1].boundary.max" in caught.value.field


def test_geometry_open_mesh(sample_bundle):
    data = copy.deepcopy(sample_bundle["scene_dict"])
    data["media"][0]["boundary"] = {
        "type": "mesh",
        "vertices": [
            [0.0, 0.0, 0.0],
            [1.0, 0.0, 0.0],
            [0.0, 1.0, 0.0],
            [0.0, 0.0, 1.0],
        ],
        "faces": [[0, 1, 2], [0, 1, 3], [0, 2, 3]],
    }
    with pytest.raises(PipelineError) as caught:
        validate_scene_dict(data)
    assert caught.value.code == E_GEOM
    assert "boundary.faces" in caught.value.field


@pytest.mark.parametrize(
    ("mutator", "field_part"),
    [
        (lambda data: data["media"][0].__setitem__("ior", [1.0, 1.5, 1.5]), "media[0].ior"),
        (lambda data: data["media"][0].__setitem__("absorption", [-0.1, 0.0, 0.0]), "media[0].absorption"),
        (lambda data: data["media"][1].__setitem__("scattering", [0.1, 0.1]), "media[1].scattering"),
        (lambda data: data["lights"][0].__setitem__("radiance", [1.0, 1.0]), "lights[0].radiance"),
        (lambda data: data["lights"][0].__setitem__("anisotropy", [0.0, 2.0, 0.0]), "lights[0].anisotropy"),
        (
            lambda data: data["camera"]["trajectory"][1].__setitem__("time", 0.0),
            "camera.trajectory",
        ),
    ],
)
def test_parameter_errors(sample_bundle, mutator, field_part):
    data = copy.deepcopy(sample_bundle["scene_dict"])
    mutator(data)
    with pytest.raises(PipelineError) as caught:
        validate_scene_dict(data)
    assert caught.value.code == E_PARA
    assert field_part in caught.value.field


def test_render_parameter_errors():
    with pytest.raises(PipelineError) as caught:
        RenderConfig(step_size=0.0)
    assert caught.value.code == E_PARA
    assert caught.value.field == "render.step_size"

    with pytest.raises(PipelineError) as caught:
        RenderConfig(sampling_strategy="unknown")
    assert caught.value.code == E_PARA
    assert caught.value.field == "render.sampling_strategy"


def test_scene_copy_preserves_hash(scene):
    assert scene.copy().content_hash == scene.content_hash

