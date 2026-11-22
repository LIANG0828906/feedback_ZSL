import json

import pytest

from ocr_pipeline.errors import VersionError
from ocr_pipeline.model import compare_results
from ocr_pipeline.schemas.loader import detect_version, load_document, load_file


def _docs(dataset, image_id="sample_000"):
    out = {}
    for version in ("1.0", "2.0", "3.0"):
        path = dataset / "sample_results" / version / f"{image_id}.json"
        out[version] = json.loads(path.read_text(encoding="utf-8"))
    return out


def test_all_versions_map_to_same_internal_model(dataset):
    docs = _docs(dataset)
    results = {v: load_document(d) for v, d in docs.items()}
    assert {r.source_version for r in results.values()} == {"1.0", "2.0", "3.0"}
    # v1 cannot carry per-char confidence; everything else must agree.
    diff = compare_results(results["2.0"], results["3.0"])
    assert diff is None
    diff = compare_results(results["1.0"], results["3.0"],
                           ignore_fields={"char_conf"})
    assert diff is None
    assert all(ln.char_conf is None for ln in results["1.0"].lines)
    assert all(ln.char_conf is not None for ln in results["2.0"].lines)


def test_source_version_preserved(dataset):
    docs = _docs(dataset)
    for version, doc in docs.items():
        assert load_document(doc).source_version == version


def test_unknown_version_raises_e_ver():
    with pytest.raises(VersionError) as exc:
        detect_version({"version": "9.9"})
    assert exc.value.code == "E_VER"
    assert exc.value.field_path == "version"
    with pytest.raises(VersionError) as exc:
        detect_version({"meta": {"schema": 7}})
    assert exc.value.field_path == "meta.schema"
    with pytest.raises(VersionError):
        detect_version({"nothing": "here"})


def test_missing_required_field_raises_e_ver_with_path(dataset):
    doc = _docs(dataset)["1.0"]
    del doc["lines"][0]["box"]
    with pytest.raises(VersionError) as exc:
        load_document(doc)
    assert exc.value.field_path == "lines[0].box"

    doc = _docs(dataset)["2.0"]
    del doc["regions"][0]["char_confidence"]
    with pytest.raises(VersionError) as exc:
        load_document(doc)
    assert exc.value.field_path == "regions[0].char_confidence"

    doc = _docs(dataset)["3.0"]
    del doc["meta"]["default_lang"]
    with pytest.raises(VersionError) as exc:
        load_document(doc)
    assert exc.value.field_path == "meta.default_lang"


def test_no_silent_defaults_for_reading_order(dataset):
    doc = _docs(dataset)["1.0"]
    doc["reading_order"] = [0, 0, 1]  # not a permutation
    with pytest.raises(VersionError) as exc:
        load_document(doc)
    assert exc.value.field_path == "reading_order"


def test_load_file_roundtrip(dataset):
    result = load_file(dataset / "sample_results" / "3.0" / "sample_002.json")
    assert result.image_id == "sample_002"
    assert result.width > 0 and result.height > 0
