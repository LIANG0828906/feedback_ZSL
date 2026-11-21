import json
import os
from dataclasses import replace
from pathlib import Path

import pytest

from spectral_bake import acceptance
from spectral_bake.batch import load_latest_valid
from spectral_bake.config import load_config
from spectral_bake.engine import run_bake
from spectral_bake.errors import (
    BudgetExceeded,
    BusyError,
    RecoveryInvalid,
    SpectrumInvalid,
)
from spectral_bake.samplegen import generate_samples
from spectral_bake.scheduler import Scheduler
from spectral_bake.spectrum import load_material, load_materials

REPO = Path(__file__).resolve().parent.parent


@pytest.fixture()
def samples(tmp_path):
    out = tmp_path / "samples"
    generate_samples(out)
    config = load_config(out / "config.json")
    materials = load_materials(out / "materials.json", config.grid)
    return out, config, materials


# --- requirement 1: spectral validation ---

def test_non_monotonic_wavelengths_located(samples):
    _, config, _ = samples
    raw = {
        "id": "bad",
        "type": "spectral",
        "wavelengths_nm": [400.0, 500.0, 450.0],
        "reflectance": [0.1, 0.2, 0.3],
    }
    with pytest.raises(SpectrumInvalid) as exc:
        load_material(0, raw, config.grid)
    assert exc.value.code == "E_SPECTRUM_INVALID"
    assert exc.value.context["sample_index"] == 2
    assert exc.value.context["wavelength_nm"] == 450.0


def test_out_of_range_value_located(samples):
    _, config, _ = samples
    raw = {
        "id": "bad",
        "type": "spectral",
        "wavelengths_nm": [400.0, 500.0, 600.0],
        "reflectance": [0.1, 1.7, 0.3],
    }
    with pytest.raises(SpectrumInvalid) as exc:
        load_material(0, raw, config.grid)
    assert exc.value.context["sample_index"] == 1
    assert exc.value.context["value"] == 1.7


def test_missing_segment_located(samples):
    _, config, _ = samples
    raw = {
        "id": "bad",
        "type": "spectral",
        "wavelengths_nm": [420.0, 500.0, 600.0],
        "reflectance": [0.1, 0.2, 0.3],
    }
    with pytest.raises(SpectrumInvalid) as exc:
        load_material(0, raw, config.grid)
    assert exc.value.context["sample_index"] == 0
    assert exc.value.context["wavelength_nm"] == 380.0


# --- requirement 5: parallelism independence ---

def test_parallelism_independence(tmp_path):
    result = acceptance.check_parallelism_independence(tmp_path)
    assert result["ok"], result


# --- requirement 4: ordered degradation + budget refusal ---

def test_ordered_degradation(tmp_path):
    result = acceptance.check_ordered_degradation(tmp_path)
    assert result["ok"], result
    assert result["stages_applied"][0] == "reduce_sphere_samples"
    assert result["budget_exceeded_raised"]
    assert result["previous_batch_retained"]


# --- requirement 3: E_BUSY on duplicate batch start ---

def test_busy_on_duplicate_batch():
    sched = Scheduler(parallelism=2, global_memory_bytes=1 << 20, trace=lambda r: None)
    tasks = [{"submission_index": 0, "memory_bytes": 1}]

    def reentrant(task):
        with pytest.raises(BusyError):
            sched.run_batch("b1", tasks, worker=lambda t: None)
        return "ok"

    assert sched.run_batch("b1", tasks, worker=reentrant) == ["ok"]


# --- requirement 6: incremental rebake ---

def test_incremental_single_material_change(samples):
    out, config, materials = samples
    run_bake(config, materials, batch_id="v1")
    raw_list = json.loads((out / "materials.json").read_text())
    raw_list[0]["reflectance"][10] = min(1.0, raw_list[0]["reflectance"][10] + 0.1)
    (out / "materials.json").write_text(json.dumps(raw_list))
    materials2 = load_materials(out / "materials.json", config.grid)
    manifest = run_bake(config, materials2, batch_id="v2")
    assert manifest["affected_count"] == 1
    assert manifest["reused_count"] == len(materials) - 1


def test_white_point_change_rebakes_all_and_reports_delta(samples):
    out, config, materials = samples
    run_bake(config, materials, batch_id="v1")
    config_d50 = replace(config, white_point="D50")
    manifest = run_bake(config_d50, materials, batch_id="v2")
    assert manifest["affected_count"] == len(materials)
    assert manifest["reused_count"] == 0
    assert manifest["max_code_delta_vs_previous"] > 0


# --- requirement 7: kill recovery ---

def test_kill_recovery(tmp_path):
    result = acceptance.check_kill_recovery(tmp_path)
    assert result["ok"], result
    assert result["journal_entries_before_recovery"] >= 3
    assert result["recovered_equals_reference"]
    assert result["broken_chain_detected"]


def test_missing_manifest_field_rejected(samples):
    out, config, materials = samples
    run_bake(config, materials, batch_id="v1")
    path = out / "snapshots" / "batch-v1.json"
    manifest = json.loads(path.read_text())
    del manifest["hash_chain"]
    path.write_text(json.dumps(manifest))
    with pytest.raises(RecoveryInvalid):
        load_latest_valid(out / "snapshots")


# --- requirement 8: GUI (offscreen) ---

def test_gui_offscreen(samples):
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    out, config, materials = samples
    manifest = run_bake(config, materials, batch_id="gui")
    from spectral_bake.gui import create_app

    app, window = create_app(
        out / "snapshots" / "batch-gui.json", out / "snapshots" / "trace.jsonl"
    )
    assert window.windowTitle() == "Spectral Material Preview"
    pixmap = window.grab()
    assert not pixmap.isNull()
    assert pixmap.width() > 0
