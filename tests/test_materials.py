from __future__ import annotations

import pytest

from spectral_editor.errors import BusinessError, E_MATERIAL_INVALID
from spectral_editor.materials import parse_material, parse_material_document
from spectral_editor.models import GridSpec


def _spectral_document(**overrides):
    material = {
        "id": "m1",
        "kind": "spectral",
        "name": "Material 1",
        "targets": ["srgb", "display-p3"],
        "truth_source": "reference-spectrophotometer",
        "spectrum": {
            "wavelengths": [400.0, 410.0, 420.0, 430.0],
            "interval_nm": 10.0,
            "values": [0.1, 0.2, 0.3, 0.4],
        },
    }
    material.update(overrides)
    return {"materials": [material]}


def test_spectral_resampling_records_source_and_count():
    grid = GridSpec(400.0, 430.0, 5.0)
    definitions = parse_material_document(_spectral_document(), grid)
    assert len(definitions) == 1
    spectrum = definitions[0].params["spectrum"]
    assert spectrum["original_sample_count"] == 4
    assert len(spectrum["resampled_values"]) == grid.sample_count
    assert spectrum["truth_source"] == "reference-spectrophotometer"
    assert spectrum["resampled_values"][1] == pytest.approx(0.15)


def test_unordered_wavelengths_are_rejected_with_field_path():
    document = _spectral_document()
    document["materials"][0]["spectrum"]["wavelengths"] = [400.0, 420.0, 410.0, 430.0]
    with pytest.raises(BusinessError) as caught:
        parse_material_document(document, GridSpec(400.0, 430.0, 5.0))
    assert caught.value.code == E_MATERIAL_INVALID
    assert "materials[0].spectrum.wavelengths[2]" == caught.value.path


def test_missing_spectrum_interval_is_rejected():
    document = _spectral_document()
    del document["materials"][0]["spectrum"]["interval_nm"]
    with pytest.raises(BusinessError) as caught:
        parse_material_document(document, GridSpec(400.0, 430.0, 5.0))
    assert caught.value.code == E_MATERIAL_INVALID
    assert caught.value.path == "materials[0].spectrum.interval_nm"


def test_out_of_range_reflectance_is_rejected():
    document = _spectral_document()
    document["materials"][0]["spectrum"]["values"][2] = 1.2
    with pytest.raises(BusinessError) as caught:
        parse_material_document(document, GridSpec(400.0, 430.0, 5.0))
    assert caught.value.code == E_MATERIAL_INVALID
    assert caught.value.path == "materials[0].spectrum.values[2]"


def test_interval_mismatch_is_rejected():
    document = _spectral_document()
    document["materials"][0]["spectrum"]["interval_nm"] = 12.0
    with pytest.raises(BusinessError) as caught:
        parse_material_document(document, GridSpec(400.0, 430.0, 5.0))
    assert caught.value.code == E_MATERIAL_INVALID
    assert caught.value.path.startswith("materials[0].spectrum.wavelengths[")


def test_invalid_display_material_is_rejected():
    document = {
        "materials": [
            {
                "id": "display",
                "kind": "display",
                "name": "Display",
                "targets": ["srgb"],
                "color_space": "srgb",
                "encoded": [0.1, 1.1, 0.2],
            }
        ]
    }
    with pytest.raises(BusinessError) as caught:
        parse_material_document(document, GridSpec(380.0, 780.0, 5.0))
    assert caught.value.code == E_MATERIAL_INVALID
    assert caught.value.path == "materials[0].encoded[1]"


def test_duplicate_material_ids_are_rejected():
    raw = _spectral_document()["materials"][0]
    with pytest.raises(BusinessError) as caught:
        parse_material_document({"materials": [raw, dict(raw)]}, GridSpec(400.0, 430.0, 5.0))
    assert caught.value.code == E_MATERIAL_INVALID
    assert caught.value.path == "materials[1].id"


def test_single_material_path_override_is_used():
    raw = _spectral_document()["materials"][0]
    raw["spectrum"]["values"][0] = -0.1
    with pytest.raises(BusinessError) as caught:
        parse_material(raw, 0, GridSpec(400.0, 430.0, 5.0), path="edit")
    assert caught.value.path == "edit.spectrum.values[0]"
