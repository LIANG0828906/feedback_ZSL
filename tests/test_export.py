import json

import pytest

from ocr_pipeline.errors import VersionError
from ocr_pipeline.model import ImageResult, compare_results
from ocr_pipeline.schemas.exporter import export_bytes, export_document
from ocr_pipeline.schemas.loader import load_document
from ocr_pipeline import engine
from ocr_pipeline.generator import gt_regions_of


def _result(dataset, image_id="sample_001"):
    gt = json.loads(
        (dataset / "ground_truth" / f"{image_id}.json").read_text(encoding="utf-8")
    )
    lines = engine.detect_and_recognize(image_id, gt_regions_of(gt))
    return ImageResult(image_id, gt["width"], gt["height"], lines, "3.0")


def test_export_reload_equivalence_all_versions(dataset):
    result = _result(dataset)
    for version in ("1.0", "2.0", "3.0"):
        payload, warnings = export_bytes(result, version)
        reloaded = load_document(json.loads(payload.decode("utf-8")))
        ignore = {"char_conf"} if version == "1.0" else set()
        assert warnings if version == "1.0" else not warnings
        diff = compare_results(result, reloaded, ignore_fields=ignore)
        assert diff is None, (version, diff)


def test_lossy_report_contents(dataset):
    result = _result(dataset)
    _doc, warnings = export_document(result, "1.0")
    assert warnings, "v1.0 export must report dropped per-char confidence"
    for warning in warnings:
        assert warning.code == "E_LOSSY"
        assert warning.field_path.startswith("lines[")
        assert warning.field_path.endswith("char_confidence")
        assert isinstance(warning.original_value, list)
        assert "dropped" in warning.disposition


def test_export_is_byte_deterministic(dataset):
    result = _result(dataset)
    for version in ("1.0", "2.0", "3.0"):
        first, _ = export_bytes(result, version)
        second, _ = export_bytes(result, version)
        assert first == second


def test_unknown_target_version_raises_e_ver(dataset):
    result = _result(dataset)
    with pytest.raises(VersionError):
        export_bytes(result, "4.0")
