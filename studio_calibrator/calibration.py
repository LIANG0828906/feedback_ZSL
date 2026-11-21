from __future__ import annotations

import math
from typing import Dict, Iterable, List, Optional, Tuple

from .models import (
    BudgetBand,
    CalibrationResult,
    CalibrationSetting,
    CalibrationStep,
    PhaseFinding,
    ScenarioConfig,
)
from .budget import phase_residual_floor
from .util import linear_to_db


CONSTRAINT_ORDER = (
    "phase_polarity",
    "target_suppression",
    "gain_cap",
    "main_gain_preserved",
)


def _round_gain_up(value_db: float, step_db: float) -> float:
    if value_db == 0.0:
        return 0.0
    return math.ceil((value_db - 1e-12) / step_db) * step_db


def solve_calibration(
    budget: Iterable[BudgetBand],
    phase_findings: Iterable[PhaseFinding],
    config: ScenarioConfig,
    speakers: Tuple[str, ...],
    receivers: Tuple[str, ...],
    main_receivers: Optional[Dict[str, str]] = None,
) -> CalibrationResult:
    main_map = main_receivers or {speaker: speaker for speaker in speakers}
    ambiguous = {
        finding.pair_key
        for finding in phase_findings
        if finding.kind == "polarity_flip_candidate"
        or (
            finding.kind == "group_delay_discontinuity"
            and finding.confidence < config.phase.ambiguity_confidence
        )
    }
    settings: List[CalibrationSetting] = []
    steps: List[CalibrationStep] = []
    conflicts: List[Dict[str, object]] = []
    step_index = 0
    for band_budget in budget:
        entry_map = {
            (entry.source, entry.receiver): entry for entry in band_budget.entries
        }
        for receiver in receivers:
            for source in speakers:
                entry = entry_map[(source, receiver)]
                is_main = main_map[source] == receiver
                if is_main:
                    setting = CalibrationSetting(
                        band_id=entry.band_id,
                        source=source,
                        receiver=receiver,
                        enabled=False,
                        gain_db=0.0,
                        polarity=1,
                        target_suppression_db=0.0,
                        achieved_suppression_db=0.0,
                        phase_deg=0.0,
                        phase_cos_abs=1.0,
                        hit_target=True,
                        main_gain_unchanged=True,
                        within_gain_cap=True,
                        feasible=True,
                        reason="main channel gain fixed at 0 dB",
                    )
                    settings.append(setting)
                    step_index += 1
                    steps.append(
                        CalibrationStep(
                            order=step_index,
                            constraint="main_gain_preserved",
                            band_id=entry.band_id,
                            source=source,
                            receiver=receiver,
                            target_suppression_db=0.0,
                            achieved_suppression_db=0.0,
                            hit=True,
                            detail="main gain unchanged",
                        )
                    )
                    continue
                pair_key = f"{source}|{receiver}"
                phase_hit = pair_key not in ambiguous
                step_index += 1
                steps.append(
                    CalibrationStep(
                        order=step_index,
                        constraint="phase_polarity",
                        band_id=entry.band_id,
                        source=source,
                        receiver=receiver,
                        target_suppression_db=entry.target_suppression_db,
                        achieved_suppression_db=None,
                        hit=phase_hit,
                        detail=(
                            "polarity selected from locally resolved coupling phase"
                            if phase_hit
                            else "phase evidence is ambiguous"
                        ),
                    )
                )
                polarity = int(entry.polarity_hint) if phase_hit else 1
                enabled = entry.required_gain_db > 0.0
                if enabled:
                    rounded_depth = _round_gain_up(
                        entry.required_gain_db, config.calibration.gain_step_db
                    )
                    desired_gain_db = min(
                        rounded_depth, config.max_cancellation_gain_db
                    )
                else:
                    desired_gain_db = 0.0
                floor_amplitude = phase_residual_floor(
                    entry.coupling_amplitude, entry.phase_cos_abs
                )
                depth_amplitude = (
                    entry.coupling_amplitude * 10.0 ** (-desired_gain_db / 20.0)
                    if enabled
                    else entry.coupling_amplitude
                )
                residual = (
                    max(depth_amplitude, floor_amplitude)
                    if enabled
                    else entry.coupling_amplitude
                )
                achieved = -linear_to_db(
                    max(residual, config.calibration.residual_floor),
                    floor_db=-240.0,
                )
                hit_target = achieved + 1e-9 >= config.target_suppression_db
                within_cap = (
                    entry.required_gain_db
                    <= config.max_cancellation_gain_db + 1e-9
                )
                feasible = phase_hit and hit_target and within_cap
                reason = "all constraints satisfied"
                if not phase_hit:
                    reason = "phase evidence cannot distinguish polarity from boundary jump"
                elif not hit_target:
                    reason = "target suppression not reached under available phase/gain"
                elif not within_cap:
                    reason = "required cancellation gain exceeds cap"
                step_index += 1
                steps.append(
                    CalibrationStep(
                        order=step_index,
                        constraint="target_suppression",
                        band_id=entry.band_id,
                        source=source,
                        receiver=receiver,
                        target_suppression_db=config.target_suppression_db,
                        achieved_suppression_db=achieved,
                        hit=hit_target,
                        detail=reason,
                    )
                )
                step_index += 1
                steps.append(
                    CalibrationStep(
                        order=step_index,
                        constraint="gain_cap",
                        band_id=entry.band_id,
                        source=source,
                        receiver=receiver,
                        target_suppression_db=config.target_suppression_db,
                        achieved_suppression_db=achieved,
                        hit=within_cap,
                        detail=(
                            f"requested gain {desired_gain_db:.6f} dB, "
                            f"cap {config.max_cancellation_gain_db:.6f} dB"
                        ),
                    )
                )
                settings.append(
                    CalibrationSetting(
                        band_id=entry.band_id,
                        source=source,
                        receiver=receiver,
                        enabled=enabled,
                        gain_db=desired_gain_db,
                        polarity=polarity,
                        target_suppression_db=config.target_suppression_db,
                        achieved_suppression_db=achieved,
                        phase_deg=entry.phase_deg,
                        phase_cos_abs=entry.phase_cos_abs,
                        hit_target=hit_target,
                        main_gain_unchanged=True,
                        within_gain_cap=within_cap,
                        feasible=feasible,
                        reason=reason,
                    )
                )
                if not feasible:
                    conflicts.append(
                        {
                            "code": "E_CALIB_INFEASIBLE",
                            "source": source,
                            "receiver": receiver,
                            "band_id": entry.band_id,
                            "failed_constraints": [
                                name
                                for name, hit in (
                                    ("phase_polarity", phase_hit),
                                    ("target_suppression", hit_target),
                                    ("gain_cap", within_cap),
                                )
                                if not hit
                            ],
                            "required_gain_db": entry.required_gain_db,
                            "max_gain_db": config.max_cancellation_gain_db,
                            "achievable_suppression_db": achieved,
                            "target_suppression_db": config.target_suppression_db,
                        }
                    )
    feasible = not conflicts
    return CalibrationResult(
        feasible=feasible,
        publishable=feasible,
        settings=tuple(settings),
        steps=tuple(steps),
        conflicts=tuple(conflicts),
        constraint_order=CONSTRAINT_ORDER,
        error_code=None if feasible else "E_CALIB_INFEASIBLE",
    )
