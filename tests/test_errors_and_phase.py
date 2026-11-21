from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest
import soundfile as sf

from studio_calibrator.alignment import align_pair
from studio_calibrator.budget import build_budget, build_route_matrix
from studio_calibrator.calibration import solve_calibration
from studio_calibrator.config import load_config, parse_config
from studio_calibrator.engine import AnalysisEngine, RecomputeManager
from studio_calibrator.errors import (
    E_BAND_UNRESOLVED,
    E_BUDGET_SHORT,
    E_CALIB_INFEASIBLE,
    E_PAIR_MISSING,
    BusinessError,
)
from studio_calibrator.geometry import GeometryManifest, PairGeometry, load_geometry
from studio_calibrator.measurement_io import (
    MeasurementManifest,
    PairMeasurement,
    load_manifest,
)
from studio_calibrator.models import (
    BudgetBand,
    BudgetEntry,
    CouplingEstimate,
    PhaseFinding,
)
from studio_calibrator.phase import analyze_phase_continuity
from studio_calibrator.snapshot import SnapshotStore


def _manager(sample: Path, root: Path) -> RecomputeManager:
    return RecomputeManager(
        AnalysisEngine(sample / "manifest.json", sample / "geometry.json"),
        SnapshotStore(root),
    )


def test_missing_geometry_distance_is_pair_missing(
    sample: Path, tmp_path: Path
) -> None:
    payload = json.loads((sample / "geometry.json").read_text(encoding="utf-8"))
    victim = next(
        item
        for item in payload["pairs"]
        if item["source"] == "FR" and item["receiver"] == "BL"
    )
    del victim["distance_m"]
    path = tmp_path / "geometry_missing.json"
    path.write_text(
        json.dumps(payload, sort_keys=True) + "\n", encoding="utf-8"
    )
    with pytest.raises(BusinessError) as caught:
        load_geometry(path)
    assert caught.value.code == E_PAIR_MISSING
    assert caught.value.source == "FR"
    assert caught.value.receiver == "BL"


def test_empty_component_is_pair_missing(sample: Path, tmp_path: Path) -> None:
    reference = np.zeros(512, dtype=np.float64)
    reference[10:40] = 0.2
    reference_path = tmp_path / "reference.wav"
    empty_path = tmp_path / "empty.wav"
    sf.write(str(reference_path), (reference * 32768).astype(np.int16), 48000, subtype="PCM_16")
    sf.write(str(empty_path), np.zeros(512, dtype=np.int16), 48000, subtype="PCM_16")
    pair = PairMeasurement(
        source="FL",
        receiver="FL",
        path=empty_path,
        path_label="empty.wav",
        channel=0,
        layout="mono",
        file_channels=1,
    )
    manifest = MeasurementManifest(
        root=tmp_path,
        sample_rate=48000,
        speakers=("FL",),
        receivers=("FL",),
        main_receivers={"FL": "FL"},
        stimuli={"FL": reference_path},
        pairs=(pair,),
        layout="mono",
        layout_description="test",
        digest="test",
    )
    geometry = GeometryManifest(
        path=tmp_path / "geometry.json",
        speed_of_sound_m_s=343.0,
        distance_basis="test",
        pairs={
            ("FL", "FL"): PairGeometry(
                source="FL",
                receiver="FL",
                distance_m=1.0,
                position_distance_m=1.0,
                distance_delta_m=0.0,
                speed_of_sound_m_s=343.0,
                basis="test",
            )
        },
        digest="test",
    )
    with pytest.raises(BusinessError) as caught:
        align_pair(manifest, geometry, pair, reference, "FL")
    assert caught.value.code == E_PAIR_MISSING
    assert caught.value.source == "FL"
    assert caught.value.receiver == "FL"


def test_unresolved_band_reports_band_and_valid_samples(
    sample: Path, tmp_path: Path
) -> None:
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["min_band_samples"] = 100000
    manager = _manager(sample, tmp_path / "snapshots_band_error")
    with pytest.raises(BusinessError) as caught:
        manager.recompute(parse_config(raw), tmp_path / "trace.jsonl")
    assert caught.value.code == E_BAND_UNRESOLVED
    assert caught.value.band == "B0"
    assert caught.value.details["valid_samples"] < 100000
    assert manager.query() is None


def test_budget_short_is_explicit_and_calibration_infeasible(
    sample: Path, tmp_path: Path
) -> None:
    raw = json.loads((sample / "config.json").read_text(encoding="utf-8"))
    raw["target_suppression_db"] = 40.0
    raw["max_cancellation_gain_db"] = 2.0
    manager = _manager(sample, tmp_path / "snapshots_short")
    state = manager.recompute(parse_config(raw), tmp_path / "trace.jsonl")
    codes = {error["code"] for error in state.errors}
    assert E_BUDGET_SHORT in codes
    assert E_CALIB_INFEASIBLE in codes
    assert state.status == "infeasible"
    short_entries = [
        entry
        for band in state.budget
        for entry in band["entries"]
        if entry["status"] == "short"
    ]
    assert short_entries
    assert any(
        entry["required_gain_db"] > entry["max_gain_db"]
        or entry["achievable_suppression_db"] < entry["target_suppression_db"]
        for entry in short_entries
    )
    assert all(
        entry["target_suppression_db"] == 40.0
        for band in state.budget
        for entry in band["entries"]
        if entry["source"] != entry["receiver"]
    )
    assert state.calibration["publishable"] is False


def _estimate(
    band_id: str,
    low_hz: float,
    phase_deg: float,
    group_delay: float = 0.0,
    source: str = "FL",
    receiver: str = "FR",
) -> CouplingEstimate:
    return CouplingEstimate(
        source=source,
        receiver=receiver,
        band_id=band_id,
        low_hz=low_hz,
        high_hz=low_hz + 100.0,
        strength_db=-20.0,
        strength_ci_db=(-20.2, -19.8),
        phase_deg=phase_deg,
        phase_ci_deg=(phase_deg - 2.0, phase_deg + 2.0),
        coherence=0.99,
        group_delay_samples=group_delay,
        valid_samples=4800,
        active_bins=100,
        quantization_relative=0.001,
        leakage_fraction=0.02,
        transition_fraction=0.05,
        estimator_version="test",
    )


def test_phase_wrap_and_polarity_candidate_are_distinguished(sample: Path) -> None:
    config = load_config(sample / "config.json")
    wrap_estimates = [
        _estimate("B0", 20.0, 170.0),
        _estimate("B1", 120.0, -170.0),
        _estimate("B2", 500.0, 160.0),
        _estimate("B3", 2000.0, -160.0),
    ]
    findings, summary = analyze_phase_continuity(wrap_estimates, config.phase)
    assert any(item.kind == "boundary_phase_wrap" for item in findings)
    assert not any(item.kind == "polarity_flip_candidate" for item in findings)
    assert summary["ambiguous_pairs"] == []

    polarity_estimates = [
        _estimate("B0", 20.0, 0.0),
        _estimate("B1", 120.0, 180.0),
        _estimate("B2", 500.0, 0.0),
        _estimate("B3", 2000.0, 180.0),
    ]
    findings, _ = analyze_phase_continuity(polarity_estimates, config.phase)
    candidates = [
        item for item in findings if item.kind == "polarity_flip_candidate"
    ]
    assert candidates
    assert "true_polarity_reversal" in candidates[0].candidate_causes
    assert "crossover_boundary_phase_jump" in candidates[0].candidate_causes


def test_group_delay_discontinuity_marks_ambiguity(sample: Path) -> None:
    config = load_config(sample / "config.json")
    estimates = [
        _estimate("B0", 20.0, 5.0, group_delay=0.0),
        _estimate("B1", 120.0, 8.0, group_delay=3.2),
    ]
    findings, summary = analyze_phase_continuity(estimates, config.phase)
    finding = next(
        item for item in findings if item.kind == "group_delay_discontinuity"
    )
    assert finding.confidence < config.phase.ambiguity_confidence
    assert "geometry_delay_residual" in finding.candidate_causes
    assert summary["ambiguous_pairs"] == ["FL|FR"]


def test_ambiguous_polarity_blocks_calibration(sample: Path) -> None:
    config = load_config(sample / "config.json")
    entry = BudgetEntry(
        source="FL",
        receiver="FR",
        band_id="B0",
        coupling_db=-20.0,
        coupling_amplitude=0.1,
        phase_deg=180.0,
        phase_cos_abs=1.0,
        target_suppression_db=12.0,
        required_gain_db=0.0,
        max_gain_db=12.0,
        achievable_suppression_db=20.0,
        residual_amplitude=0.1,
        residual_energy=0.01,
        residual_crosstalk_db=-20.0,
        suppression_db=20.0,
        status="ok",
        reason="already meets target",
        polarity_hint=-1,
    )
    budget = BudgetBand(
        band_id="B0",
        entries=(entry,),
        per_receiver_residual_energy={"FR": 0.01},
        per_receiver_suppression_db={"FR": 20.0},
        short_bands=(),
    )
    finding = PhaseFinding(
        source="FL",
        receiver="FR",
        kind="polarity_flip_candidate",
        band_from="B0",
        band_to="B1",
        phase_from_deg=0.0,
        phase_to_deg=180.0,
        wrapped_delta_deg=180.0,
        raw_delta_deg=180.0,
        confidence=0.52,
        severity="error",
        candidate_causes=(
            "true_polarity_reversal",
            "crossover_boundary_phase_jump",
        ),
    )
    result = solve_calibration(
        [budget],
        [finding],
        config,
        speakers=("FL",),
        receivers=("FR",),
    )
    assert result.feasible is False
    assert result.publishable is False
    assert result.error_code == "E_CALIB_INFEASIBLE"
    assert result.steps[0].constraint == "phase_polarity"
    assert result.steps[0].hit is False
    assert "phase_polarity" in result.conflicts[0]["failed_constraints"]


def test_main_receiver_mapping_is_used_by_route_budget_and_calibration(
    sample: Path,
) -> None:
    config = load_config(sample / "config.json")
    estimates = [
        _estimate("B0", 20.0, 0.0, source="FL", receiver="MIC_FL"),
        _estimate("B0", 20.0, 0.0, source="FL", receiver="MIC_FR"),
    ]
    main_map = {"FL": "MIC_FL"}
    routes = build_route_matrix(
        estimates,
        config,
        speakers=("FL",),
        receivers=("MIC_FL", "MIC_FR"),
        main_receivers=main_map,
    )
    assert routes["B0"].matrix["MIC_FL"]["FL"]["is_main"] is True
    assert routes["B0"].matrix["MIC_FL"]["FL"]["amplitude"] == 1.0
    assert routes["B0"].matrix["MIC_FR"]["FL"]["is_main"] is False
    budget = build_budget(
        estimates,
        config,
        speakers=("FL",),
        receivers=("MIC_FL", "MIC_FR"),
        residual_floor=config.calibration.residual_floor,
        main_receivers=main_map,
    )
    entries = {
        (entry.source, entry.receiver): entry for entry in budget["B0"].entries
    }
    assert entries[("FL", "MIC_FL")].status == "main"
    calibration = solve_calibration(
        [budget["B0"]],
        [],
        config,
        speakers=("FL",),
        receivers=("MIC_FL", "MIC_FR"),
        main_receivers=main_map,
    )
    main_setting = next(
        setting
        for setting in calibration.settings
        if setting.receiver == "MIC_FL"
    )
    assert main_setting.gain_db == 0.0
    assert main_setting.polarity == 1
    assert main_setting.main_gain_unchanged is True
