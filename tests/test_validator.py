import json

import pytest

from ocr_pipeline.pipeline import Pipeline, PipelineConfig
from ocr_pipeline.validator import ValidationFailure, validate_all, validate_image


def _run(dataset, **overrides):
    config = PipelineConfig(data_dir=dataset, lines_per_interval=1, **overrides)
    Pipeline(config).run()


def test_validator_accepts_clean_run(dataset):
    _run(dataset)
    reports = validate_all(dataset)
    assert len(reports) == 6
    assert all(r["lines"] > 0 for r in reports)


def test_validator_rejects_incomplete_run(dataset):
    _run(dataset)
    manifest_path = dataset / "checkpoints" / "sample_000" / "manifest.json"
    manifest = json.loads(manifest_path.read_text())
    manifest["committed_intervals"] = 0
    manifest_path.write_text(json.dumps(manifest))
    with pytest.raises(ValidationFailure) as exc:
        validate_image(dataset, "sample_000")
    assert exc.value.field == "progress"


def test_validator_localizes_tampered_text(dataset):
    _run(dataset)
    record_path = dataset / "checkpoints" / "sample_001" / "intervals" / "0000.json"
    record = json.loads(record_path.read_text())
    record["lines"][0]["text"] = "FORGED"
    record_path.write_text(json.dumps(record))
    with pytest.raises(ValidationFailure) as exc:
        validate_image(dataset, "sample_001")
    assert exc.value.image_id == "sample_001"
    assert exc.value.field == "text"


def test_validator_localizes_tampered_confidence(dataset):
    _run(dataset)
    record_path = dataset / "checkpoints" / "sample_001" / "intervals" / "0000.json"
    record = json.loads(record_path.read_text())
    record["lines"][0]["char_conf"][0] = 0.01
    record_path.write_text(json.dumps(record))
    with pytest.raises(ValidationFailure) as exc:
        validate_image(dataset, "sample_001")
    assert exc.value.field == "char_conf"
