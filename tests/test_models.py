"""Model registry state machine: loading, isolated failure, reload, wait."""

import json

import pytest

from gateway.errors import ErrorCode, GatewayError
from gateway.models import (
    STATE_FAILED,
    STATE_LOADING,
    STATE_READY,
    ModelRegistry,
)
from gateway.protocol import load_protocol_mappings


def _registry(model_dir):
    specs = load_protocol_mappings(model_dir / "protocol_mapping.json")
    return ModelRegistry(model_dir, specs), specs


def test_starts_in_loading_state(generated_dir):
    registry, _ = _registry(generated_dir / "models")
    assert registry.state_of("v3").state == STATE_LOADING
    assert registry.state_of("v4").state == STATE_LOADING


def test_successful_load_marks_ready(generated_dir):
    registry, _ = _registry(generated_dir / "models")
    registry.load_blocking("v3")
    state = registry.state_of("v3")
    assert state.state == STATE_READY and state.loaded_at is not None
    assert registry.get_ready_session("v3") is not None


def test_one_version_failure_does_not_block_other(generated_dir, tmp_path):
    broken_dir = tmp_path / "broken"
    broken_dir.mkdir()
    specs_raw = json.loads((generated_dir / "models" / "protocol_mapping.json").read_text())
    # v3 points at a missing file, v4 copied intact
    specs_raw["v3"]["model_file"] = "missing.onnx"
    (broken_dir / "protocol_mapping.json").write_text(json.dumps(specs_raw))
    import shutil

    shutil.copy(
        generated_dir / "models" / specs_raw["v4"]["model_file"],
        broken_dir / specs_raw["v4"]["model_file"],
    )

    registry, _ = _registry(broken_dir)
    registry.load_blocking("v3")
    registry.load_blocking("v4")

    assert registry.state_of("v3").state == STATE_FAILED
    assert "FileNotFoundError" in registry.state_of("v3").last_error
    assert registry.state_of("v4").state == STATE_READY

    with pytest.raises(GatewayError) as exc:
        registry.get_ready_session("v3")
    assert exc.value.code == ErrorCode.MODEL_LOAD_FAILED
    registry.get_ready_session("v4")  # healthy version still serves


def test_reload_recovers_failed_version(generated_dir, tmp_path):
    broken_dir = tmp_path / "reload"
    broken_dir.mkdir()
    import shutil

    mapping_src = generated_dir / "models" / "protocol_mapping.json"
    specs_raw = json.loads(mapping_src.read_text())
    (broken_dir / "protocol_mapping.json").write_text(json.dumps(specs_raw))
    registry, _ = _registry(broken_dir)

    registry.load_blocking("v3")
    assert registry.state_of("v3").state == STATE_FAILED

    shutil.copy(
        generated_dir / "models" / specs_raw["v3"]["model_file"],
        broken_dir / specs_raw["v3"]["model_file"],
    )
    state = registry.reload("v3")
    assert state.state == STATE_READY and state.load_attempts == 2


def test_wait_until_ready_distinguishes_failure_and_timeout(generated_dir):
    registry, _ = _registry(generated_dir / "models")
    # still loading -> timeout error code
    with pytest.raises(GatewayError) as exc:
        registry.wait_until_ready("v3", timeout=0.02)
    assert exc.value.code == ErrorCode.READY_WAIT_TIMEOUT

    registry.load_blocking("v3")
    registry.wait_until_ready("v3", timeout=0.1)  # returns immediately
