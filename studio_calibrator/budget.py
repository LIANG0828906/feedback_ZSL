from __future__ import annotations

import math
from typing import Dict, Iterable, Optional, Tuple

from .models import (
    BudgetBand,
    BudgetEntry,
    CouplingEstimate,
    RoutedBand,
    ScenarioConfig,
)
from .util import db_to_linear, linear_to_db, wrap_deg


def build_route_matrix(
    estimates: Iterable[CouplingEstimate],
    config: ScenarioConfig,
    speakers: Tuple[str, ...],
    receivers: Tuple[str, ...],
    main_receivers: Optional[Dict[str, str]] = None,
) -> Dict[str, RoutedBand]:
    main_map = main_receivers or {speaker: speaker for speaker in speakers}
    by_band: Dict[str, list[CouplingEstimate]] = {}
    for estimate in estimates:
        by_band.setdefault(estimate.band_id, []).append(estimate)
    result: Dict[str, RoutedBand] = {}
    for band in config.bands:
        if band.id not in by_band:
            continue
        matrix: Dict[str, Dict[str, Dict[str, object]]] = {}
        receiver_energy: Dict[str, float] = {}
        for receiver in receivers:
            matrix[receiver] = {}
            total = 0.0
            for source in speakers:
                matches = [
                    item
                    for item in by_band.get(band.id, [])
                    if item.source == source and item.receiver == receiver
                ]
                if not matches:
                    continue
                estimate = matches[0]
                is_main = main_map[source] == receiver
                amplitude = 1.0 if is_main else db_to_linear(estimate.strength_db)
                phase = 0.0 if is_main else estimate.phase_deg
                matrix[receiver][source] = {
                    "amplitude": amplitude,
                    "strength_db": linear_to_db(amplitude),
                    "phase_deg": phase,
                    "is_main": is_main,
                }
                if not is_main:
                    total += amplitude**2
            receiver_energy[receiver] = total
        result[band.id] = RoutedBand(
            band=band,
            matrix=matrix,
            per_receiver_energy=receiver_energy,
        )
    return result

def residual_for_gain(
    coupling_amplitude: float,
    phase_cos_abs: float,
    gain_amplitude: float,
) -> float:
    square = (
        coupling_amplitude**2
        + gain_amplitude**2
        - 2.0 * coupling_amplitude * gain_amplitude * phase_cos_abs
    )
    return math.sqrt(max(0.0, square))


def phase_residual_floor(
    coupling_amplitude: float,
    phase_cos_abs: float,
) -> float:
    return coupling_amplitude * math.sqrt(
        max(0.0, 1.0 - phase_cos_abs**2)
    )


def build_budget(
    estimates: Iterable[CouplingEstimate],
    config: ScenarioConfig,
    speakers: Tuple[str, ...],
    receivers: Tuple[str, ...],
    residual_floor: float,
    main_receivers: Optional[Dict[str, str]] = None,
) -> Dict[str, BudgetBand]:
    main_map = main_receivers or {speaker: speaker for speaker in speakers}
    by_band_pair: Dict[tuple[str, str, str], CouplingEstimate] = {
        (item.source, item.receiver, item.band_id): item for item in estimates
    }
    target_amplitude = db_to_linear(-config.target_suppression_db)
    result: Dict[str, BudgetBand] = {}
    estimate_bands = {key[2] for key in by_band_pair}
    for band in config.bands:
        if band.id not in estimate_bands:
            continue
        entries = []
        for receiver in receivers:
            for source in speakers:
                estimate = by_band_pair[(source, receiver, band.id)]
                is_main = main_map[source] == receiver
                if is_main:
                    amplitude = 1.0
                    phase = 0.0
                    phase_cos_abs = 1.0
                    entry = BudgetEntry(
                        source=source,
                        receiver=receiver,
                        band_id=band.id,
                        coupling_db=0.0,
                        coupling_amplitude=1.0,
                        phase_deg=0.0,
                        phase_cos_abs=1.0,
                        target_suppression_db=0.0,
                        required_gain_db=0.0,
                        max_gain_db=config.max_cancellation_gain_db,
                        achievable_suppression_db=0.0,
                        residual_amplitude=1.0,
                        residual_energy=1.0,
                        residual_crosstalk_db=0.0,
                        suppression_db=0.0,
                        status="main",
                        reason="main channel gain is fixed",
                        polarity_hint=1,
                    )
                    entries.append(entry)
                    continue
                amplitude = db_to_linear(estimate.strength_db)
                phase_cos_abs = abs(math.cos(math.radians(estimate.phase_deg)))
                polarity_hint = 1 if math.cos(math.radians(estimate.phase_deg)) >= 0 else -1
                floor_amplitude = phase_residual_floor(
                    amplitude, phase_cos_abs
                )
                required_gain_db = (
                    0.0
                    if amplitude <= target_amplitude
                    else 20.0
                    * math.log10(amplitude / max(target_amplitude, 1e-300))
                )
                budget_depth = min(
                    required_gain_db, config.max_cancellation_gain_db
                )
                achievable_residual = max(
                    amplitude * 10.0 ** (-budget_depth / 20.0),
                    floor_amplitude,
                )
                achievable_suppression = -linear_to_db(
                    max(achievable_residual, residual_floor), floor_db=-240.0
                )
                if amplitude <= target_amplitude:
                    status = "ok"
                    reason = "coupling already meets target"
                elif floor_amplitude > target_amplitude:
                    status = "short"
                    reason = "phase mismatch floor prevents the target"
                elif required_gain_db > config.max_cancellation_gain_db + 1e-12:
                    status = "short"
                    reason = "required cancellation depth exceeds configured cap"
                else:
                    status = "ok"
                    reason = "target reachable within gain cap"
                residual_energy = achievable_residual**2
                residual_crosstalk_db = linear_to_db(
                    max(achievable_residual, residual_floor)
                )
                entries.append(
                    BudgetEntry(
                        source=source,
                        receiver=receiver,
                        band_id=band.id,
                        coupling_db=float(estimate.strength_db),
                        coupling_amplitude=float(amplitude),
                        phase_deg=float(estimate.phase_deg),
                        phase_cos_abs=float(phase_cos_abs),
                        target_suppression_db=config.target_suppression_db,
                        required_gain_db=float(required_gain_db),
                        max_gain_db=config.max_cancellation_gain_db,
                        achievable_suppression_db=float(achievable_suppression),
                        residual_amplitude=float(achievable_residual),
                        residual_energy=float(residual_energy),
                        residual_crosstalk_db=float(residual_crosstalk_db),
                        suppression_db=float(achievable_suppression),
                        status=status,
                        reason=reason,
                        polarity_hint=polarity_hint,
                    )
                )
        residual_by_receiver: Dict[str, float] = {}
        suppression_by_receiver: Dict[str, float] = {}
        for receiver in receivers:
            cross = [
                entry
                for entry in entries
                if entry.receiver == receiver
                and main_map[entry.source] != receiver
            ]
            energy = sum(entry.residual_energy for entry in cross)
            residual_by_receiver[receiver] = energy
            suppression_by_receiver[receiver] = -linear_to_db(
                math.sqrt(max(energy, residual_floor**2)), floor_db=-240.0
            )
        short = tuple(
            f"{entry.source}|{entry.receiver}"
            for entry in entries
            if entry.status == "short"
        )
        result[band.id] = BudgetBand(
            band_id=band.id,
            entries=tuple(entries),
            per_receiver_residual_energy=residual_by_receiver,
            per_receiver_suppression_db=suppression_by_receiver,
            short_bands=short,
        )
    return result
