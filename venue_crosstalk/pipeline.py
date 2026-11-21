"""Dependency-linked analysis pipeline with cached incremental recomputation."""

from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .analysis import align_loaded_pair, load_pair_measurement
from .cache import CacheStore
from .calibration import (
    PairBandCalibration,
    build_residual_budget,
    calibrate_pair,
)
from .config import AnalysisConfig, config_from_dict
from .dsp import BandEstimate
from .errors import (
    E_CALIB_INFEASIBLE,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_GEOMETRY_INVALID,
    E_POLARITY_UNKNOWN,
    VenueError,
)
from .geometry import resolve_distance
from .models import PairSpec, Workspace, load_workspace
from .phase import PhaseContinuity, analyze_phase_continuity
from .trace import TraceLogger
from .utils import json_ready, sha256_array, sha256_json

PUBLISH_SCHEMA = "venue-crosstalk-publish/v1"
ALGORITHM_VERSION = 1


@dataclass
class _RunContext:
    operation: str
    workspace: Workspace
    config: AnalysisConfig
    previous: dict[str, Any] | None
    changed_bands: list[str]
    changed_pairs: list[str]
    removed_pairs: list[str]
    force_all: bool
    reasons: list[dict[str, Any]]
    previous_estimates: dict[tuple[str, str], dict[str, Any]]


class AnalysisPipeline:
    """Run a full or incremental crosstalk analysis for one workspace."""

    def __init__(
        self,
        workspace: str | Path,
        *,
        cache_root: str | Path | None = None,
        trace_path: str | Path | None = None,
        reset_trace: bool = False,
    ) -> None:
        self.workspace_root = Path(workspace).resolve()
        self.cache = CacheStore(
            Path(cache_root) if cache_root is not None else self.workspace_root / "cache"
        )
        resolved_trace = (
            Path(trace_path)
            if trace_path is not None
            else self.workspace_root / "logs" / "trace.jsonl"
        )
        self.trace = TraceLogger(resolved_trace, reset=reset_trace)
        self._stats: dict[str, Any] = {}
        self._operation = "analyze"

    def _reset_stats(self) -> None:
        self._stats = {
            "cache_hits": 0,
            "cache_misses": 0,
            "reused_nodes": [],
            "recomputed_nodes": [],
        }

    def _record_cache(
        self,
        node: str,
        scope: str,
        lookup_hit: bool,
    ) -> None:
        if lookup_hit:
            self._stats["cache_hits"] += 1
            self._stats["reused_nodes"].append({"node": node, "scope": scope})
        else:
            self._stats["cache_misses"] += 1
            self._stats["recomputed_nodes"].append({"node": node, "scope": scope})

    def _compare_previous(
        self,
        workspace: Workspace,
        config: AnalysisConfig,
        previous: dict[str, Any] | None,
        operation: str,
    ) -> _RunContext:
        current_band_names = [band.name for band in config.bands]
        current_pair_ids = [pair.pair_id for pair in workspace.pairs]
        changed_bands: list[str] = []
        changed_pairs: list[str] = []
        removed_pairs: list[str] = []
        reasons: list[dict[str, Any]] = []
        previous_estimates: dict[tuple[str, str], dict[str, Any]] = {}
        force_all = False
        if not previous:
            changed_bands = current_band_names
            changed_pairs = current_pair_ids
            snapshot_path = workspace.root / "publish" / "snapshot.json"
            reasons.append(
                {
                    "node": "analysis",
                    "scope": "workspace",
                    "reason": (
                        "published-snapshot-unreadable-conservative-recompute"
                        if snapshot_path.exists()
                        else "initial-run-no-published-state"
                    ),
                }
            )
        elif previous.get("schema") != PUBLISH_SCHEMA:
            force_all = True
            changed_bands = current_band_names
            changed_pairs = current_pair_ids
            reasons.append(
                {
                    "node": "analysis",
                    "scope": "workspace",
                    "reason": "published-schema-incompatible-conservative-recompute",
                }
            )
        else:
            old_state = previous.get("workspace", {})
            old_config_data = previous.get("effective_config", {})
            old_pairs = {
                str(item.get("id")): item
                for item in old_state.get("pairs", [])
                if isinstance(item, dict)
            }
            current_pairs = {pair.pair_id: pair for pair in workspace.pairs}
            changed_pairs = sorted(
                pair_id
                for pair_id, pair in current_pairs.items()
                if pair_id not in old_pairs
                or sha256_json(pair.to_dict()) != old_pairs[pair_id].get("fingerprint")
            )
            removed_pairs = sorted(set(old_pairs) - set(current_pairs))
            try:
                old_config = config_from_dict(old_config_data)
            except VenueError:
                force_all = True
                changed_bands = current_band_names
                changed_pairs = current_pair_ids
                reasons.append(
                    {
                        "node": "analysis",
                        "scope": "workspace",
                        "reason": "previous-config-unreadable-conservative-recompute",
                    }
                )
                old_config = None
            if old_config is not None:
                if old_config.sample_rate != config.sample_rate:
                    force_all = True
                    changed_bands = current_band_names
                    reasons.append(
                        {
                            "node": "alignment",
                            "scope": "all-pairs",
                            "reason": "sample-rate-changed",
                        }
                    )
                if old_config.transition_hz != config.transition_hz:
                    changed_bands = current_band_names
                    reasons.append(
                        {
                            "node": "band_coupling",
                            "scope": "all-bands",
                            "reason": "filter-transition-width-changed",
                        }
                    )
                old_bands = {band.name: band for band in old_config.bands}
                current_bands = {band.name: band for band in config.bands}
                band_scope_changed = False
                for name, band in current_bands.items():
                    old = old_bands.get(name)
                    if (
                        old is None
                        or old.low_hz != band.low_hz
                        or old.high_hz != band.high_hz
                    ):
                        if name not in changed_bands:
                            changed_bands.append(name)
                        band_scope_changed = True
                        reasons.append(
                            {
                                "node": "band_coupling",
                                "scope": name,
                                "reason": (
                                    "new-band" if old is None else "band-boundaries-changed"
                                ),
                            }
                        )
                for name in sorted(set(old_bands) - set(current_bands)):
                    reasons.append(
                        {
                            "node": "band_coupling",
                            "scope": name,
                            "reason": "band-removed",
                        }
                    )
                old_geometry = old_state.get("geometry_hash")
                if old_geometry != workspace.geometry.fingerprint:
                    force_all = True
                    changed_bands = current_band_names
                    reasons.append(
                        {
                            "node": "alignment",
                            "scope": "all-pairs",
                            "reason": "geometry-or-distance-basis-changed",
                        }
                    )
                if old_config.polarity != config.polarity:
                    reasons.append(
                        {
                            "node": "phase_continuity",
                            "scope": "all-pairs",
                            "reason": "polarity-threshold-config-changed",
                        }
                    )
                if old_config.target_suppression_db != config.target_suppression_db:
                    reasons.append(
                        {
                            "node": "calibration",
                            "scope": "all-pairs-all-bands",
                            "reason": "target-suppression-changed",
                        }
                    )
                if (
                    old_config.max_cancel_gain != config.max_cancel_gain
                    or old_config.max_primary_suppression_db
                    != config.max_primary_suppression_db
                    or old_config.primary_leakage != config.primary_leakage
                ):
                    reasons.append(
                        {
                            "node": "calibration",
                            "scope": "all-pairs-all-bands",
                            "reason": "calibration-constraints-changed",
                        }
                    )
                for section in previous.get("bands", []):
                    band_name = str(section.get("spec", {}).get("name"))
                    for estimate in section.get("coupling", []):
                        previous_estimates[
                            (str(estimate["pair"]), band_name)
                        ] = estimate
                # Pair changes imply all bands for the affected pair.
                if changed_pairs:
                    band_scope_changed = True
                if not band_scope_changed:
                    changed_bands = []
            if changed_pairs:
                reasons.append(
                    {
                        "node": "alignment/band_coupling",
                        "scope": ",".join(changed_pairs),
                        "reason": "pair-spec-or-measurement-reference-changed",
                    }
                )
        return _RunContext(
            operation=operation,
            workspace=workspace,
            config=config,
            previous=previous,
            changed_bands=sorted(set(changed_bands)),
            changed_pairs=sorted(set(changed_pairs)),
            removed_pairs=removed_pairs,
            force_all=force_all,
            reasons=reasons,
            previous_estimates=previous_estimates,
        )

    def _effective_config(
        self,
        workspace: Workspace,
        *,
        config: AnalysisConfig | None,
        target_suppression_db: float | None,
        bands: Iterable[dict[str, Any] | tuple[str, float, float]] | None,
    ) -> AnalysisConfig:
        effective = config if config is not None else workspace.config
        if effective.sample_rate != workspace.config.sample_rate:
            raise VenueError(
                E_CONFIG_INVALID,
                "override config sample rate must match workspace manifest",
                {
                    "workspace_sample_rate": workspace.config.sample_rate,
                    "override_sample_rate": effective.sample_rate,
                },
            )
        if bands is not None:
            effective = effective.with_bands(bands)
        if target_suppression_db is not None:
            target = float(target_suppression_db)
            if not math.isfinite(target) or not (0.0 <= target <= 120.0):
                raise VenueError(
                    E_CONFIG_INVALID,
                    "target suppression must be finite and in [0, 120] dB",
                    {"target_suppression_db": target},
                )
            effective = effective.with_target_suppression(target)
        return effective

    def _cache_get(
        self,
        *,
        node: str,
        scope: str,
        dependency_hash: str,
        force: bool,
    ):
        lookup = self.cache.get(
            node,
            scope,
            dependency_hash,
            force=force,
        )
        self._record_cache(node, scope, lookup.hit)
        return lookup

    def _cache_put(
        self,
        node: str,
        scope: str,
        dependency_hash: str,
        payload: dict[str, Any],
    ) -> str:
        _, payload_hash = self.cache.put(node, scope, dependency_hash, payload)
        return payload_hash

    def run(
        self,
        *,
        operation: str = "analyze",
        config: AnalysisConfig | None = None,
        target_suppression_db: float | None = None,
        bands: Iterable[dict[str, Any] | tuple[str, float, float]] | None = None,
        strict: bool = False,
        reset_trace: bool = False,
        cancel_event: Any | None = None,
    ) -> dict[str, Any]:
        def check_cancel(stage: str) -> None:
            if cancel_event is not None and cancel_event.is_set():
                self.trace.emit(
                    operation=operation,
                    node="cancel",
                    cache_hit=False,
                    reason=f"cooperative-cancel-at-{stage}",
                )
                raise VenueError(
                    E_CANCELLED,
                    "analysis cancelled cooperatively",
                    {"stage": stage},
                )

        self._operation = operation
        if reset_trace:
            self.trace = TraceLogger(self.trace.path, reset=True)
        self._reset_stats()
        check_cancel("start")
        workspace = load_workspace(self.workspace_root)
        effective_config = self._effective_config(
            workspace,
            config=config,
            target_suppression_db=target_suppression_db,
            bands=bands,
        )
        previous = self.cache.read_snapshot(self.workspace_root)
        previous_measurements = (
            {}
            if not previous
            else {
                str(item["pair"]): str(item.get("content_hash", ""))
                for item in previous.get("measurements", [])
                if isinstance(item, dict) and "pair" in item
            }
        )
        context = self._compare_previous(
            workspace,
            effective_config,
            previous,
            operation,
        )
        for reason in context.reasons:
            self.trace.emit(
                operation=operation,
                node="incremental_plan",
                cache_hit=False,
                reason=reason["reason"],
                data=reason,
            )

        # Geometry failures abort before any expensive spectral work and carry the
        # exact missing speaker/pickup entry.
        for pair in workspace.pairs:
            resolve_distance(pair, workspace.geometry, effective_config.sample_rate)

        pairs_by_id = {pair.pair_id: pair for pair in workspace.pairs}
        estimates_by_pair: dict[str, dict[str, BandEstimate]] = {
            pair_id: {} for pair_id in pairs_by_id
        }
        alignments: list[dict[str, Any]] = []
        measurement_entries: list[dict[str, Any]] = []
        estimation_changes: list[dict[str, Any]] = []
        coupling_hashes: dict[tuple[str, str], str] = {}

        for pair in sorted(workspace.pairs, key=lambda item: item.pair_id):
            check_cancel("pair-start")
            loaded = load_pair_measurement(workspace, pair)
            previous_measurement_hash = previous_measurements.get(pair.pair_id)
            if (
                previous_measurement_hash
                and previous_measurement_hash != loaded.content_hash
                and pair.pair_id not in context.changed_pairs
            ):
                context.changed_pairs.append(pair.pair_id)
                context.changed_pairs.sort()
                reason = {
                    "node": "measurement/alignment/band_coupling",
                    "scope": pair.pair_id,
                    "reason": "measurement-content-hash-changed",
                }
                context.reasons.append(reason)
                self.trace.emit(
                    operation=operation,
                    node="incremental_plan",
                    pair=pair.pair_id,
                    cache_hit=False,
                    reason=reason["reason"],
                    data=reason,
                )
            measurement_dep = sha256_json(
                {
                    "pair": pair.to_dict(),
                    "pickup_hash": loaded.pickup_wav_hash,
                    "reference_hash": loaded.reference_wav_hash,
                    "sample_rate": effective_config.sample_rate,
                    "algorithm": ALGORITHM_VERSION,
                }
            )
            measurement_force = context.force_all or pair.pair_id in context.changed_pairs
            measurement_lookup = self._cache_get(
                node="measurement",
                scope=pair.pair_id,
                dependency_hash=measurement_dep,
                force=measurement_force,
            )
            measurement_payload = loaded.summary()
            if measurement_lookup.hit:
                assert measurement_lookup.payload is not None
                measurement_payload = measurement_lookup.payload
            else:
                self._cache_put(
                    "measurement",
                    pair.pair_id,
                    measurement_dep,
                    measurement_payload,
                )
            measurement_entries.append(measurement_payload)
            self.trace.emit(
                operation=operation,
                node="measurement",
                pair=pair.pair_id,
                cache_hit=measurement_lookup.hit,
                reason=measurement_lookup.reason,
                content_hash=measurement_payload.get("content_hash"),
                data={
                    "layout": pair.layout,
                    "sample_rate": effective_config.sample_rate,
                },
            )

            alignment_dep = sha256_json(
                {
                    "measurement_hash": loaded.content_hash,
                    "geometry_hash": workspace.geometry.fingerprint,
                    "sample_rate": effective_config.sample_rate,
                    "algorithm": ALGORITHM_VERSION,
                }
            )
            alignment_lookup = self._cache_get(
                node="geometry_alignment",
                scope=pair.pair_id,
                dependency_hash=alignment_dep,
                force=context.force_all or pair.pair_id in context.changed_pairs,
            )
            aligned = None
            if alignment_lookup.hit:
                assert alignment_lookup.payload is not None
                alignment_payload = alignment_lookup.payload
                aligned_hash = str(alignment_payload["aligned_content_hash"])
            else:
                aligned = align_loaded_pair(workspace, loaded)
                aligned_hash = sha256_json(
                    [
                        sha256_array(aligned.reference),
                        sha256_array(aligned.pickup),
                    ]
                )
                alignment_payload = {
                    **aligned.summary(),
                    "aligned_content_hash": aligned_hash,
                }
                self._cache_put(
                    "geometry_alignment",
                    pair.pair_id,
                    alignment_dep,
                    alignment_payload,
                )
            alignments.append(alignment_payload)
            self.trace.emit(
                operation=operation,
                node="geometry_alignment",
                pair=pair.pair_id,
                cache_hit=alignment_lookup.hit,
                reason=alignment_lookup.reason,
                content_hash=aligned_hash,
                data={
                    "arrival_samples": alignment_payload["arrival_samples"],
                    "alignment_offset_samples": alignment_payload[
                        "alignment_offset_samples"
                    ],
                    "residual_offset_samples": alignment_payload[
                        "residual_offset_samples"
                    ],
                    "distance_basis": alignment_payload["distance_basis"],
                },
            )

            for band in effective_config.bands:
                check_cancel("band-start")
                coupling_dep = sha256_json(
                    {
                        "aligned_hash": aligned_hash,
                        "band_hash": effective_config.band_fingerprint(band),
                        "pair": pair.pair_id,
                        "algorithm": ALGORITHM_VERSION,
                    }
                )
                force_coupling = context.force_all or (
                    pair.pair_id in context.changed_pairs
                    and band.name in context.changed_bands
                )
                lookup = self._cache_get(
                    node="band_coupling",
                    scope=f"{pair.pair_id}|{band.name}",
                    dependency_hash=coupling_dep,
                    force=force_coupling,
                )
                if lookup.hit:
                    assert lookup.payload is not None
                    estimate_payload = lookup.payload
                    estimate = BandEstimate.from_dict(estimate_payload)
                else:
                    if aligned is None:
                        aligned = align_loaded_pair(workspace, loaded)
                        recomputed_hash = sha256_json(
                            [
                                sha256_array(aligned.reference),
                                sha256_array(aligned.pickup),
                            ]
                        )
                        if recomputed_hash != aligned_hash:
                            raise VenueError(
                                "E_CACHE_INVALID",
                                "alignment content hash changed while reusing cached path",
                                {"pair": pair.pair_id},
                            )
                    from .analysis import analyze_band_coupling

                    estimate = analyze_band_coupling(
                        pair.pair_id,
                        aligned.reference,
                        aligned.pickup,
                        band,
                        effective_config,
                    )
                    estimate_payload = estimate.to_dict()
                    self._cache_put(
                        "band_coupling",
                        f"{pair.pair_id}|{band.name}",
                        coupling_dep,
                        estimate_payload,
                    )
                    previous_estimate = context.previous_estimates.get(
                        (pair.pair_id, band.name)
                    )
                    if previous_estimate is not None:
                        old_mag = float(previous_estimate["coupling_magnitude"])
                        old_phase = float(previous_estimate["phase_deg"])
                        old_unc = float(
                            previous_estimate["magnitude_uncertainty_db"]
                        )
                        phase_delta = (estimate.phase_deg - old_phase + 180.0) % 360.0 - 180.0
                        estimation_changes.append(
                            {
                                "pair": pair.pair_id,
                                "band": band.name,
                                "old_interval_hz": [
                                    previous_estimate["band_low_hz"],
                                    previous_estimate["band_high_hz"],
                                ],
                                "new_interval_hz": [band.low_hz, band.high_hz],
                                "old_coupling_magnitude": old_mag,
                                "new_coupling_magnitude": estimate.magnitude,
                                "magnitude_delta_db": (
                                    20.0 * math.log10(max(estimate.magnitude, 1e-15))
                                    - 20.0 * math.log10(max(old_mag, 1e-15))
                                ),
                                "phase_delta_deg": phase_delta,
                                "old_magnitude_uncertainty_db": old_unc,
                                "new_magnitude_uncertainty_db": (
                                    estimate.magnitude_uncertainty_db
                                ),
                                "magnitude_uncertainty_change_db": (
                                    estimate.magnitude_uncertainty_db - old_unc
                                ),
                                "old_phase_uncertainty_deg": float(
                                    previous_estimate["phase_uncertainty_deg"]
                                ),
                                "new_phase_uncertainty_deg": (
                                    estimate.phase_uncertainty_deg
                                ),
                                "phase_uncertainty_change_deg": (
                                    estimate.phase_uncertainty_deg
                                    - float(previous_estimate["phase_uncertainty_deg"])
                                ),
                            }
                        )
                estimates_by_pair[pair.pair_id][band.name] = estimate
                coupling_hashes[(pair.pair_id, band.name)] = estimate.content_hash
                self.trace.emit(
                    operation=operation,
                    node="band_coupling",
                    pair=pair.pair_id,
                    band=band.name,
                    cache_hit=lookup.hit,
                    reason=lookup.reason,
                    content_hash=estimate.content_hash,
                    data={
                        "coupling_magnitude": estimate.magnitude,
                        "coupling_db": estimate.magnitude_db,
                        "phase_deg": estimate.phase_deg,
                        "magnitude_uncertainty_db": (
                            estimate.magnitude_uncertainty_db
                        ),
                        "phase_uncertainty_deg": estimate.phase_uncertainty_deg,
                    },
                )

        phases: dict[str, PhaseContinuity] = {}
        phase_payloads: dict[str, dict[str, Any]] = {}
        for pair_id in sorted(pairs_by_id):
            pair_estimates = list(estimates_by_pair[pair_id].values())
            if not pair_estimates:
                continue
            phase_dep = sha256_json(
                {
                    "estimates": sorted(
                        [
                            [band_name, estimate.content_hash]
                            for band_name, estimate in estimates_by_pair[pair_id].items()
                        ]
                    ),
                    "polarity": effective_config.polarity.to_dict(),
                    "algorithm": ALGORITHM_VERSION,
                }
            )
            lookup = self._cache_get(
                node="phase_continuity",
                scope=pair_id,
                dependency_hash=phase_dep,
                force=context.force_all,
            )
            if lookup.hit:
                assert lookup.payload is not None
                phase_payload = lookup.payload
                phase = PhaseContinuity.from_dict(phase_payload)
            else:
                phase = analyze_phase_continuity(
                    sorted(pair_estimates, key=lambda item: item.band.low_hz),
                    effective_config.polarity,
                )
                phase_payload = phase.to_dict()
                self._cache_put(
                    "phase_continuity",
                    pair_id,
                    phase_dep,
                    phase_payload,
                )
            phases[pair_id] = phase
            phase_payloads[pair_id] = phase_payload
            self.trace.emit(
                operation=operation,
                node="phase_continuity",
                pair=pair_id,
                cache_hit=lookup.hit,
                reason=lookup.reason,
                content_hash=phase.content_hash,
                data={
                    "verdict": phase.verdict,
                    "code": phase.code,
                    "inversion_bands": phase.inversion_bands,
                    "boundary_wraps": len(phase.boundary_wraps),
                    "phase_jumps": len(phase.phase_jumps),
                    "group_delay_discontinuities": len(
                        phase.group_delay_discontinuities
                    ),
                },
            )

        calibration_payloads: dict[tuple[str, str], dict[str, Any]] = {}
        calibration_objects: dict[tuple[str, str], PairBandCalibration] = {}
        calibration_hashes: dict[tuple[str, str], str] = {}
        for pair_id in sorted(pairs_by_id):
            pair = pairs_by_id[pair_id]
            if pair.source == pair.pickup:
                continue
            phase = phases[pair_id]
            for band in effective_config.bands:
                estimate = estimates_by_pair[pair_id][band.name]
                calibration_dep = sha256_json(
                    {
                        "estimate_hash": estimate.content_hash,
                        "phase_calibration_signature": {
                            "verdict": phase.verdict,
                            "code": phase.code,
                            "inversion_bands": phase.inversion_bands,
                        },
                        "target_db": effective_config.target_suppression_db,
                        "max_gain": effective_config.max_cancel_gain,
                        "max_primary_db": (
                            effective_config.max_primary_suppression_db
                        ),
                        "default_primary_leakage": effective_config.primary_leakage,
                        "pair_primary_leakage": pair.primary_leakage,
                        "algorithm": ALGORITHM_VERSION,
                    }
                )
                lookup = self._cache_get(
                    node="calibration",
                    scope=f"{pair_id}|{band.name}",
                    dependency_hash=calibration_dep,
                    force=context.force_all,
                )
                if lookup.hit:
                    assert lookup.payload is not None
                    calibration_payload = lookup.payload
                    calibration = PairBandCalibration.from_dict(calibration_payload)
                else:
                    calibration = calibrate_pair(
                        estimate,
                        phase,
                        effective_config,
                        primary_leakage=pair.primary_leakage,
                    )
                    calibration_payload = calibration.to_dict()
                    self._cache_put(
                        "calibration",
                        f"{pair_id}|{band.name}",
                        calibration_dep,
                        calibration_payload,
                    )
                calibration_payloads[(pair_id, band.name)] = calibration_payload
                calibration_objects[(pair_id, band.name)] = calibration
                calibration_hashes[(pair_id, band.name)] = calibration.content_hash
                self.trace.emit(
                    operation=operation,
                    node="calibration",
                    pair=pair_id,
                    band=band.name,
                    cache_hit=lookup.hit,
                    reason=lookup.reason,
                    content_hash=calibration.content_hash,
                    data={
                        "cancel_gain_linear": calibration.cancel_gain,
                        "polarity_setting": calibration.polarity_setting,
                        "achieved_suppression_db": (
                            calibration.achieved_suppression_db
                        ),
                        "target_suppression_db": calibration.target_suppression_db,
                        "gap_db": calibration.gap_db,
                        "feasible": calibration.feasible,
                        "constraint_order": calibration.constraint_order,
                    },
                )

        pair_metadata = {
            pair.pair_id: (pair.source, pair.pickup) for pair in workspace.pairs
        }
        band_sections: list[dict[str, Any]] = []
        budget_payloads: dict[str, dict[str, Any]] = {}
        for band in effective_config.bands:
            check_cancel("matrix-budget")
            matrix_dep = sha256_json(
                {
                    "band": effective_config.band_fingerprint(band),
                    "pairs": sorted(pair_metadata.items()),
                    "coupling": sorted(
                        [
                            [pair_id, estimates_by_pair[pair_id][band.name].content_hash]
                            for pair_id in pairs_by_id
                        ]
                    ),
                    "algorithm": ALGORITHM_VERSION,
                }
            )
            matrix_lookup = self._cache_get(
                node="routing_matrix",
                scope=band.name,
                dependency_hash=matrix_dep,
                force=context.force_all,
            )
            if matrix_lookup.hit:
                assert matrix_lookup.payload is not None
                matrix_payload = matrix_lookup.payload
            else:
                matrix = self._build_matrix(
                    band,
                    workspace,
                    estimates_by_pair,
                )
                matrix_payload = {
                    "band": band.name,
                    "band_index": band.index,
                    "layout": matrix,
                    "content_hash": sha256_json(matrix),
                }
                self._cache_put(
                    "routing_matrix",
                    band.name,
                    matrix_dep,
                    matrix_payload,
                )
            self.trace.emit(
                operation=operation,
                node="routing_matrix",
                band=band.name,
                cache_hit=matrix_lookup.hit,
                reason=matrix_lookup.reason,
                content_hash=matrix_payload["content_hash"],
                data={
                    "rows": matrix_payload["layout"]["pickups"],
                    "columns": matrix_payload["layout"]["sources"],
                    "conditionally_recomputed": not matrix_lookup.hit,
                },
            )

            calibrations = [
                calibration_objects[(pair_id, band.name)]
                for pair_id in sorted(pairs_by_id)
                if (pair_id, band.name) in calibration_objects
            ]
            budget_dep = sha256_json(
                {
                    "band": band.name,
                    "target": effective_config.target_suppression_db,
                    "calibrations": sorted(
                        [
                            [pair_id, calibration_hashes[(pair_id, band.name)]]
                            for pair_id in pairs_by_id
                            if (pair_id, band.name) in calibration_hashes
                        ]
                    ),
                    "pair_metadata": sorted(pair_metadata.items()),
                    "algorithm": ALGORITHM_VERSION,
                }
            )
            budget_lookup = self._cache_get(
                node="residual_budget",
                scope=band.name,
                dependency_hash=budget_dep,
                force=context.force_all,
            )
            if budget_lookup.hit:
                assert budget_lookup.payload is not None
                budget_payload = budget_lookup.payload
            else:
                budget = build_residual_budget(
                    band.name,
                    band.index,
                    effective_config.target_suppression_db,
                    calibrations,
                    pair_metadata,
                )
                budget_payload = budget.to_dict()
                self._cache_put(
                    "residual_budget",
                    band.name,
                    budget_dep,
                    budget_payload,
                )
            budget_payloads[band.name] = budget_payload
            for pair_budget in budget_payload["pairs"]:
                self.trace.emit(
                    operation=operation,
                    node="residual_budget",
                    pair=pair_budget["pair"],
                    band=band.name,
                    cache_hit=budget_lookup.hit,
                    reason=budget_lookup.reason,
                    content_hash=budget_payload["content_hash"],
                    data={
                        "residual_magnitude": pair_budget["residual_magnitude"],
                        "residual_energy": pair_budget["residual_energy"],
                        "achieved_suppression_db": pair_budget[
                            "achieved_suppression_db"
                        ],
                        "target_suppression_db": pair_budget[
                            "target_suppression_db"
                        ],
                        "gap_db": pair_budget["gap_db"],
                        "met": pair_budget["met"],
                    },
                )
            for pickup_budget in budget_payload["pickups"]:
                self.trace.emit(
                    operation=operation,
                    node="residual_budget_pickup",
                    pair=f"*>{pickup_budget['pickup']}",
                    band=band.name,
                    cache_hit=budget_lookup.hit,
                    reason=budget_lookup.reason,
                    content_hash=budget_payload["content_hash"],
                    data=pickup_budget,
                )
            band_sections.append(
                {
                    "spec": band.to_dict(),
                    "matrix": matrix_payload,
                    "coupling": [
                        estimates_by_pair[pair_id][band.name].to_dict()
                        for pair_id in sorted(pairs_by_id)
                    ],
                    "phase": {
                        pair_id: phase_payloads[pair_id]
                        for pair_id in sorted(phase_payloads)
                    },
                    "calibration": [
                        calibration_payloads[(pair_id, band.name)]
                        for pair_id in sorted(pairs_by_id)
                        if (pair_id, band.name) in calibration_payloads
                    ],
                    "residual_budget": budget_payload,
                }
            )

        errors = self._collect_errors(
            phases,
            calibration_objects,
            effective_config,
            budget_payloads,
        )
        if strict and errors:
            first = errors[0]
            raise VenueError(
                str(first["code"]),
                str(first["message"]),
                dict(first.get("details", {})),
            )
        check_cancel("publish")
        body: dict[str, Any] = {
            "schema": PUBLISH_SCHEMA,
            "algorithm_version": ALGORITHM_VERSION,
            "status": "ok" if not errors else "attention",
            "effective_config": effective_config.to_dict(),
            "workspace": {
                "manifest_hash": workspace.manifest_fingerprint,
                "geometry_hash": workspace.geometry.fingerprint,
                "sample_rate": effective_config.sample_rate,
                "pairs": [
                    {
                        "id": pair.pair_id,
                        "source": pair.source,
                        "pickup": pair.pickup,
                        "fingerprint": sha256_json(pair.to_dict()),
                    }
                    for pair in sorted(workspace.pairs, key=lambda item: item.pair_id)
                ],
            },
            "incremental": {
                "changed_bands": context.changed_bands,
                "changed_pairs": context.changed_pairs,
                "removed_pairs": context.removed_pairs,
                "cache_hits": self._stats["cache_hits"],
                "cache_misses": self._stats["cache_misses"],
                "reused_nodes": sorted(
                    self._stats["reused_nodes"],
                    key=lambda item: (item["node"], item["scope"]),
                ),
                "recomputed_nodes": sorted(
                    self._stats["recomputed_nodes"],
                    key=lambda item: (item["node"], item["scope"]),
                ),
                "reasons": context.reasons,
            },
            "measurements": sorted(
                measurement_entries, key=lambda item: item["pair"]
            ),
            "alignments": sorted(alignments, key=lambda item: item["pair"]),
            "phase_continuity": [
                phase_payloads[pair_id] for pair_id in sorted(phase_payloads)
            ],
            "calibration": [
                calibration_payloads[key]
                for key in sorted(calibration_payloads)
            ],
            "bands": band_sections,
            "estimation_changes": sorted(
                estimation_changes,
                key=lambda item: (item["pair"], item["band"]),
            ),
            "errors": errors,
            "summary": {
                "band_count": len(effective_config.bands),
                "pair_count": len(workspace.pairs),
                "inverted_pairs": sorted(
                    pair_id
                    for pair_id, phase in phases.items()
                    if phase.verdict == "inverted"
                ),
                "unknown_polarity_pairs": sorted(
                    pair_id
                    for pair_id, phase in phases.items()
                    if phase.code == E_POLARITY_UNKNOWN
                ),
                "infeasible_calibration_pairs": sorted(
                    {
                        pair_id
                        for (pair_id, _), item in calibration_objects.items()
                        if not item.feasible
                    }
                ),
                "shortfall_bands": sorted(
                    {
                        band
                        for band, payload in budget_payloads.items()
                        if payload["shortfall_pairs"]
                        or payload["shortfall_pickups"]
                    }
                ),
                "cache_hits": self._stats["cache_hits"],
                "cache_misses": self._stats["cache_misses"],
            },
        }
        body = json_ready(body)
        semantic_body = dict(body)
        semantic_body.pop("incremental", None)
        semantic_summary = dict(semantic_body.get("summary", {}))
        semantic_summary.pop("cache_hits", None)
        semantic_summary.pop("cache_misses", None)
        semantic_body["summary"] = semantic_summary
        run_id = sha256_json(semantic_body)
        body["run_id"] = run_id
        body["content_hash"] = run_id
        self.trace.emit(
            operation=operation,
            node="publish",
            cache_hit=False,
            reason="atomic-snapshot-replacement",
            content_hash=run_id,
            data={
                "cache_hits": self._stats["cache_hits"],
                "cache_misses": self._stats["cache_misses"],
                "changed_bands": context.changed_bands,
                "status": body["status"],
            },
        )
        body["trace"] = {
            "path": str(self.trace.path.relative_to(self.workspace_root)),
            "final_chain_hash": self.trace.chain_hash,
        }
        snapshot = json_ready(body)
        self.cache.publish_atomic(self.workspace_root, snapshot)
        return snapshot

    def _build_matrix(
        self,
        band,
        workspace: Workspace,
        estimates_by_pair: dict[str, dict[str, BandEstimate]],
    ) -> dict[str, Any]:
        sources = sorted({pair.source for pair in workspace.pairs})
        pickups = sorted({pair.pickup for pair in workspace.pairs})
        source_index = {source: index for index, source in enumerate(sources)}
        pickup_index = {pickup: index for index, pickup in enumerate(pickups)}
        magnitude = [[0.0 for _ in sources] for _ in pickups]
        phase_deg = [[0.0 for _ in sources] for _ in pickups]
        for pair in workspace.pairs:
            row = pickup_index[pair.pickup]
            column = source_index[pair.source]
            estimate = estimates_by_pair[pair.pair_id].get(band.name)
            if estimate is not None:
                magnitude[row][column] = estimate.magnitude
                phase_deg[row][column] = estimate.phase_deg
        for index in range(min(len(sources), len(pickups))):
            source_name = sources[index]
            if source_name in pickup_index:
                row = pickup_index[source_name]
                if magnitude[row][index] == 0.0:
                    magnitude[row][index] = 1.0
        return {
            "sources": sources,
            "pickups": pickups,
            "magnitude": magnitude,
            "phase_deg": phase_deg,
        }

    def _collect_errors(
        self,
        phases: dict[str, PhaseContinuity],
        calibrations: dict[tuple[str, str], PairBandCalibration],
        config: AnalysisConfig,
        budgets: dict[str, dict[str, Any]],
    ) -> list[dict[str, Any]]:
        errors: list[dict[str, Any]] = []
        for pair_id in sorted(phases):
            phase = phases[pair_id]
            if phase.code == E_POLARITY_UNKNOWN:
                errors.append(
                    {
                        "code": E_POLARITY_UNKNOWN,
                        "message": (
                            "polarity inversion cannot be distinguished from a "
                            "band-boundary phase offset"
                        ),
                        "details": {
                            "pair": pair_id,
                            "candidates": phase.candidates,
                            "boundary_wraps": phase.boundary_wraps,
                            "phase_jumps": phase.phase_jumps,
                            "evidence": phase.evidence,
                        },
                    }
                )
        for (pair_id, band_name), calibration in sorted(calibrations.items()):
            if not calibration.feasible:
                errors.append(
                    {
                        "code": E_CALIB_INFEASIBLE,
                        "message": calibration.reason,
                        "details": {
                            "pair": pair_id,
                            "band": band_name,
                            "target_suppression_db": calibration.target_suppression_db,
                            "achieved_suppression_db": (
                                calibration.achieved_suppression_db
                            ),
                            "gap_db": calibration.gap_db,
                            "max_cancel_gain": config.max_cancel_gain,
                            "primary_suppression_db": (
                                calibration.primary_suppression_db
                            ),
                        },
                    }
                )
        for band_name in sorted(budgets):
            payload = budgets[band_name]
            for shortfall in payload["shortfall_pickups"]:
                if any(
                    error["code"] == E_CALIB_INFEASIBLE
                    and error["details"].get("band") == band_name
                    for error in errors
                ):
                    continue
                errors.append(
                    {
                        "code": "E_TARGET_NOT_MET",
                        "message": "pickup aggregate residual exceeds target",
                        "details": dict(shortfall),
                    }
                )
        return sorted(
            errors,
            key=lambda item: (
                item["code"],
                str(item.get("details", {}).get("pair", "")),
                str(item.get("details", {}).get("band", "")),
            ),
        )

    def latest(self) -> dict[str, Any] | None:
        return self.cache.read_snapshot(self.workspace_root)
