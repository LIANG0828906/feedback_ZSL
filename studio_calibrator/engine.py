from __future__ import annotations

import threading
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Tuple

from .alignment import align_all
from .budget import build_budget, build_route_matrix
from .calibration import solve_calibration
from .config import load_config
from .coupling import estimate_band
from .errors import (
    E_BAND_UNRESOLVED,
    E_BUDGET_SHORT,
    E_CALIB_INFEASIBLE,
    E_CONFIG_INVALID,
    E_PHASE_AMBIGUOUS,
    E_BUSY,
    E_CANCELLED,
    BusinessError,
    error_list,
)
from .geometry import load_geometry
from .incremental import RecomputePlan, build_plan
from .measurement_io import load_manifest
from .models import (
    AnalysisState,
    AlignmentResult,
    BudgetBand,
    CalibrationResult,
    CalibrationStep,
    CouplingEstimate,
    RoutedBand,
    ScenarioConfig,
    phase_difference_summary,
)
from .phase import analyze_phase_continuity
from .snapshot import SnapshotStore
from .trace import TraceLogger
from .util import (
    content_hash,
    file_hash,
    optional_cancel,
)


class AnalysisEngine:
    def __init__(self, manifest_path: Path, geometry_path: Path) -> None:
        self.manifest_path = manifest_path
        self.geometry_path = geometry_path
        self._alignment_key: Optional[str] = None
        self._alignments: Dict[str, AlignmentResult] = {}
        self._alignment_summary_hash: Optional[str] = None
        self._coupling_cache: Dict[tuple[Any, ...], CouplingEstimate] = {}
        self._route_cache: Dict[str, tuple[tuple[Any, ...], RoutedBand]] = {}
        self._budget_cache: Dict[str, tuple[tuple[Any, ...], BudgetBand]] = {}
        self._phase_cache: Dict[str, tuple[str, List[Any], Dict[str, Any]]] = {}
        self._calibration_cache: Dict[
            str, tuple[tuple[Any, ...], CalibrationResult]
        ] = {}
        self._previous_state: Optional[AnalysisState] = None
        self._previous_config: Optional[Dict[str, Any]] = None
        self._cache_stats: Dict[str, Dict[str, int]] = {}

    def publish_state(self, state: AnalysisState, config: ScenarioConfig) -> None:
        self._previous_state = state
        self._previous_config = config.to_dict()

    def _measurement_fingerprint(self, manifest: Any) -> str:
        payload = {
            "manifest_digest": manifest.digest,
            "stimuli": {
                source: file_hash(path)
                for source, path in sorted(manifest.stimuli.items())
            },
            "pairs": [
                {
                    "source": pair.source,
                    "receiver": pair.receiver,
                    "layout": pair.layout,
                    "channel": pair.channel,
                    "path": pair.path_label,
                    "content": file_hash(pair.path),
                }
                for pair in manifest.pairs
            ],
        }
        return content_hash(payload)

    def _log_cache(
        self,
        trace: TraceLogger,
        node: str,
        band: Optional[str],
        cached: bool,
        reason: str,
        pair: Optional[str] = None,
        content: Optional[str] = None,
    ) -> None:
        stats = self._cache_stats.setdefault(
            node, {"cache_hits": 0, "recomputed": 0}
        )
        stats["cache_hits" if cached else "recomputed"] += 1
        trace.emit(
            node,
            "cache_hit" if cached else "recomputed",
            pair=pair,
            band=band,
            cached=cached,
            reason=reason,
            content_hash=content,
        )

    def _validate_bands(self, config: ScenarioConfig, sample_rate: int) -> None:
        nyquist = sample_rate / 2.0
        for band in config.bands:
            if band.high_hz > nyquist:
                raise BusinessError(
                    E_CONFIG_INVALID,
                    "band high_hz exceeds Nyquist frequency",
                    band=band.id,
                    details={"high_hz": band.high_hz, "nyquist_hz": nyquist},
                )

    def run(
        self,
        config: ScenarioConfig,
        trace_path: Optional[Path],
        cancel_event: Optional[threading.Event] = None,
    ) -> AnalysisState:
        self._cache_stats = {}
        with TraceLogger(trace_path) as trace:
            state = self._run(config, trace, cancel_event)
            return state

    def _run(
        self,
        config: ScenarioConfig,
        trace: TraceLogger,
        cancel_event: Optional[threading.Event],
    ) -> AnalysisState:
        optional_cancel(cancel_event)
        manifest = load_manifest(self.manifest_path, config.measurement_layout)
        geometry = load_geometry(self.geometry_path)
        self._validate_bands(config, manifest.sample_rate)
        measurement_fingerprint = self._measurement_fingerprint(manifest)
        alignment_key = content_hash(
            {
                "manifest_digest": manifest.digest,
                "geometry_digest": geometry.digest,
                "measurement_fingerprint": measurement_fingerprint,
                "layout": config.measurement_layout,
            }
        )
        alignment_changed = self._alignment_key != alignment_key
        plan = build_plan(
            self._previous_state,
            self._previous_config,
            config,
            alignment_changed,
        )
        trace.emit(
            "dependency_plan",
            "planned",
            content_hash=content_hash(plan.to_dict()),
            details=plan.to_dict(),
        )

        optional_cancel(cancel_event)
        if alignment_changed or not self._alignments:
            alignments, scores = align_all(
                manifest, geometry, cancel_event=cancel_event
            )
            self._alignments = alignments
            self._alignment_key = alignment_key
            self._alignment_summary_hash = scores["__alignment_hash__"]
            for pair_key in sorted(alignments):
                alignment = alignments[pair_key]
                self._log_cache(
                    trace,
                    "alignment",
                    None,
                    False,
                    "alignment fingerprint changed or cache empty",
                    pair=pair_key,
                    content=alignment.hash,
                )
        else:
            for pair_key in sorted(self._alignments):
                alignment = self._alignments[pair_key]
                self._log_cache(
                    trace,
                    "alignment",
                    None,
                    True,
                    "geometry, distance basis and component hashes unchanged",
                    pair=pair_key,
                    content=alignment.hash,
                )

        optional_cancel(cancel_event)
        coupling: Dict[tuple[str, str, str], CouplingEstimate] = {}
        forced = set(plan.forced_bands)
        for pair_key in sorted(self._alignments):
            alignment = self._alignments[pair_key]
            for band in config.bands:
                optional_cancel(cancel_event)
                cache_key = (
                    pair_key,
                    band.hash,
                    alignment.hash,
                    config.estimator_version,
                )
                cached = cache_key in self._coupling_cache and band.id not in forced
                if cached:
                    estimate = self._coupling_cache[cache_key]
                    reason = "alignment hash, band hash and estimator version unchanged"
                else:
                    estimate = estimate_band(
                        alignment, band, config, cancel_event=cancel_event
                    )
                    self._coupling_cache[cache_key] = estimate
                    if band.id in forced:
                        reason = plan.forced_reasons.get(
                            band.id, "conservative dependency recomputation"
                        )
                    elif alignment_changed:
                        reason = "alignment changed"
                    else:
                        reason = "band definition or estimator input changed"
                coupling[(estimate.source, estimate.receiver, band.id)] = estimate
                self._log_cache(
                    trace,
                    "coupling",
                    band.id,
                    cached,
                    reason,
                    pair=pair_key,
                    content=estimate.hash,
                )
                trace.emit(
                    "coupling",
                    "estimate",
                    pair=pair_key,
                    band=band.id,
                    cached=cached,
                    reason=reason,
                    content_hash=estimate.hash,
                    details=estimate.to_dict(),
                )

        coupling_values = [
            coupling[key] for key in sorted(coupling, key=lambda item: (item[0], item[1], item[2]))
        ]
        previous_coupling = (
            list(self._previous_state.coupling) if self._previous_state else []
        )
        coupling_diff = phase_difference_summary(
            previous_coupling,
            [estimate.to_dict() for estimate in coupling_values],
        )
        trace.emit(
            "coupling",
            "difference_summary",
            content_hash=content_hash(coupling_diff),
            details=coupling_diff,
        )

        optional_cancel(cancel_event)
        routes: Dict[str, RoutedBand] = {}
        for band in config.bands:
            band_estimates = [
                estimate
                for estimate in coupling_values
                if estimate.band_id == band.id
            ]
            coupling_hash = content_hash(
                [estimate.to_dict() for estimate in band_estimates]
            )
            cache_key = (band.hash, coupling_hash)
            cached_entry = self._route_cache.get(band.id)
            cached = cached_entry is not None and cached_entry[0] == cache_key
            if cached:
                route = cached_entry[1]
                reason = "band coupling matrix unchanged"
            else:
                route = build_route_matrix(
                    band_estimates,
                    config,
                    manifest.speakers,
                    manifest.receivers,
                    manifest.main_receivers,
                )[band.id]
                self._route_cache[band.id] = (cache_key, route)
                reason = "band coupling matrix changed"
            routes[band.id] = route
            self._log_cache(
                trace,
                "route_matrix",
                band.id,
                cached,
                reason,
                content=route.hash,
            )

        optional_cancel(cancel_event)
        budget: Dict[str, BudgetBand] = {}
        for band in config.bands:
            route = routes[band.id]
            cache_key = (
                route.hash,
                config.target_suppression_db,
                config.max_cancellation_gain_db,
                config.calibration.residual_floor,
            )
            cached_entry = self._budget_cache.get(band.id)
            cached = cached_entry is not None and cached_entry[0] == cache_key
            if cached:
                band_budget = cached_entry[1]
                reason = "route matrix, target and gain cap unchanged"
            else:
                band_budget = build_budget(
                    [
                        estimate
                        for estimate in coupling_values
                        if estimate.band_id == band.id
                    ],
                    config,
                    manifest.speakers,
                    manifest.receivers,
                    config.calibration.residual_floor,
                    manifest.main_receivers,
                )[band.id]
                self._budget_cache[band.id] = (cache_key, band_budget)
                reason = "route matrix, target or gain cap changed"
            budget[band.id] = band_budget
            self._log_cache(
                trace,
                "budget",
                band.id,
                cached,
                reason,
                content=band_budget.hash,
            )
            for entry in band_budget.entries:
                trace.emit(
                    "budget",
                    "pair_budget",
                    pair=f"{entry.source}|{entry.receiver}",
                    band=band.id,
                    cached=cached,
                    reason=reason,
                    content_hash=entry.hash,
                    details=entry.to_dict(),
                )

        optional_cancel(cancel_event)
        phase_key = content_hash(
            {
                "coupling": [estimate.hash for estimate in coupling_values],
                "policy": config.to_dict()["phase"],
            }
        )
        cached_phase = self._phase_cache.get(phase_key)
        phase_cached = cached_phase is not None
        if phase_cached:
            phase_findings, phase_summary = cached_phase[1], cached_phase[2]
            for pair_key in sorted({item.pair_key for item in coupling_values}):
                self._log_cache(
                    trace,
                    "phase",
                    None,
                    True,
                    "all band coupling phases and phase policy unchanged",
                    pair=pair_key,
                    content=content_hash(
                        [finding.to_dict() for finding in phase_findings]
                    ),
                )
        else:
            phase_findings, phase_summary = analyze_phase_continuity(
                coupling_values, config.phase, manifest.main_receivers
            )
            self._phase_cache[phase_key] = (
                phase_key,
                phase_findings,
                phase_summary,
            )
            for pair_key in sorted({item.pair_key for item in coupling_values}):
                self._log_cache(
                    trace,
                    "phase",
                    None,
                    False,
                    "band coupling phase changed or phase policy changed",
                    pair=pair_key,
                    content=content_hash(
                        [finding.to_dict() for finding in phase_findings]
                    ),
                )
        phase_hash = content_hash([finding.to_dict() for finding in phase_findings])

        optional_cancel(cancel_event)
        calibration_bands: List[CalibrationResult] = []
        all_settings = []
        all_steps: List[CalibrationStep] = []
        all_conflicts: List[Dict[str, Any]] = []
        for band in config.bands:
            band_budget = budget[band.id]
            cache_key = (
                band_budget.hash,
                phase_hash,
                config.target_suppression_db,
                config.max_cancellation_gain_db,
                config.calibration.gain_step_db,
                config.calibration.residual_floor,
            )
            cached_entry = self._calibration_cache.get(band.id)
            cached = cached_entry is not None and cached_entry[0] == cache_key
            if cached:
                band_calibration = cached_entry[1]
                reason = "budget, phase evidence and calibration policy unchanged"
            else:
                band_calibration = solve_calibration(
                    [band_budget],
                    phase_findings,
                    config,
                    manifest.speakers,
                    manifest.receivers,
                    manifest.main_receivers,
                )
                self._calibration_cache[band.id] = (cache_key, band_calibration)
                reason = "budget, phase evidence or calibration policy changed"
            calibration_bands.append(band_calibration)
            all_settings.extend(band_calibration.settings)
            all_steps.extend(band_calibration.steps)
            all_conflicts.extend(band_calibration.conflicts)
            self._log_cache(
                trace,
                "calibration",
                band.id,
                cached,
                reason,
                content=band_calibration.hash,
            )
            for setting in band_calibration.settings:
                trace.emit(
                    "calibration",
                    "setting",
                    pair=f"{setting.source}|{setting.receiver}",
                    band=band.id,
                    cached=cached,
                    reason=reason,
                    content_hash=content_hash(setting.to_dict()),
                    details=setting.to_dict(),
                )
        renumbered_steps = tuple(
            CalibrationStep(
                order=index + 1,
                constraint=step.constraint,
                band_id=step.band_id,
                source=step.source,
                receiver=step.receiver,
                target_suppression_db=step.target_suppression_db,
                achieved_suppression_db=step.achieved_suppression_db,
                hit=step.hit,
                detail=step.detail,
            )
            for index, step in enumerate(all_steps)
        )
        feasible = all(result.feasible for result in calibration_bands)
        calibration = CalibrationResult(
            feasible=feasible,
            publishable=feasible,
            settings=tuple(all_settings),
            steps=renumbered_steps,
            conflicts=tuple(all_conflicts),
            constraint_order=calibration_bands[0].constraint_order,
            error_code=None if feasible else E_CALIB_INFEASIBLE,
        )

        errors: List[BusinessError] = []
        for band in config.bands:
            for entry in budget[band.id].entries:
                if entry.status == "short":
                    errors.append(
                        BusinessError(
                            E_BUDGET_SHORT,
                            "target suppression cannot be reached within the gain cap",
                            source=entry.source,
                            receiver=entry.receiver,
                            band=band.id,
                            severity="warning",
                            details={
                                "required_gain_db": entry.required_gain_db,
                                "max_gain_db": entry.max_gain_db,
                                "achievable_suppression_db": entry.achievable_suppression_db,
                                "target_suppression_db": entry.target_suppression_db,
                                "reason": entry.reason,
                            },
                        )
                    )
        for finding in phase_findings:
            if finding.kind == "polarity_flip_candidate" or (
                finding.kind == "group_delay_discontinuity"
                and finding.confidence < config.phase.ambiguity_confidence
            ):
                errors.append(
                    BusinessError(
                        E_PHASE_AMBIGUOUS,
                        "phase evidence cannot distinguish polarity reversal from boundary effects",
                        source=finding.source,
                        receiver=finding.receiver,
                        band=f"{finding.band_from}..{finding.band_to}",
                        severity="error",
                        details={
                            "candidates": list(finding.candidate_causes),
                            "confidence": finding.confidence,
                            "wrapped_delta_deg": finding.wrapped_delta_deg,
                            "raw_delta_deg": finding.raw_delta_deg,
                        },
                    )
                )
        if not feasible:
            errors.append(
                BusinessError(
                    E_CALIB_INFEASIBLE,
                    "calibration constraints cannot be satisfied simultaneously",
                    severity="error",
                    details={"conflicts": list(all_conflicts)},
                )
            )

        alignment_summaries = tuple(
            self._alignments[pair_key].public_dict()
            for pair_key in sorted(self._alignments)
        )
        route_values = tuple(routes[band.id].to_dict() for band in config.bands)
        budget_values = tuple(budget[band.id].to_dict() for band in config.bands)
        coupling_dicts = tuple(estimate.to_dict() for estimate in coupling_values)
        finding_dicts = tuple(finding.to_dict() for finding in phase_findings)
        calibration_dict = calibration.to_dict()
        node_hashes = {
            "alignment": content_hash(alignment_summaries),
            "coupling": content_hash(coupling_dicts),
            "route_matrix": content_hash(route_values),
            "budget": content_hash(budget_values),
            "phase": content_hash(finding_dicts),
            "calibration": content_hash(calibration_dict),
        }
        status = "ok"
        if not feasible:
            status = "infeasible"
        elif errors:
            status = "degraded"
        incremental = plan.to_dict()
        incremental["cache_stats"] = self._cache_stats
        trace.emit(
            "state",
            "candidate_complete",
            content_hash=content_hash(node_hashes),
            details={
                "status": status,
                "node_hashes": node_hashes,
                "error_codes": sorted({error.code for error in errors}),
            },
        )
        trace_hash = trace.digest_hex
        state = AnalysisState(
            schema_version=1,
            config=config.to_dict(),
            manifest_digest=manifest.digest,
            geometry_digest=geometry.digest,
            alignments=alignment_summaries,
            coupling=coupling_dicts,
            route_matrix=route_values,
            budget=budget_values,
            phase_findings=finding_dicts,
            phase_summary=phase_summary,
            calibration=calibration_dict,
            errors=tuple(error.to_dict() for error in errors),
            incremental=incremental,
            coupling_diff=coupling_diff,
            node_hashes=node_hashes,
            trace_hash=trace_hash,
            status=status,
        ).with_hash()
        return state


class RecomputeManager:
    def __init__(self, engine: AnalysisEngine, snapshots: SnapshotStore) -> None:
        self.engine = engine
        self.snapshots = snapshots
        self._lock = threading.RLock()
        self._active = False
        self._cancel_event: Optional[threading.Event] = None
        self._published: Optional[AnalysisState] = None
        self._last_snapshot: Optional[Dict[str, Any]] = None

    @property
    def busy(self) -> bool:
        with self._lock:
            return self._active

    def recompute(
        self,
        config: ScenarioConfig,
        trace_path: Optional[Path] = None,
    ) -> AnalysisState:
        cancel_event = threading.Event()
        with self._lock:
            if self._active:
                raise BusinessError(E_BUSY, "a recompute is already active")
            self._active = True
            self._cancel_event = cancel_event
        try:
            state = self.engine.run(config, trace_path, cancel_event)
            optional_cancel(cancel_event)
            with self._lock:
                if cancel_event.is_set():
                    raise BusinessError(E_CANCELLED, "recompute cancelled")
                snapshot_info = self.snapshots.commit(state)
                self.engine.publish_state(state, config)
                self._published = state
                self._last_snapshot = snapshot_info
            return state
        finally:
            with self._lock:
                self._active = False
                self._cancel_event = None

    def cancel(self) -> bool:
        with self._lock:
            if not self._active or self._cancel_event is None:
                return False
            self._cancel_event.set()
            return True

    def query(self) -> Optional[AnalysisState]:
        with self._lock:
            return self._published

    @property
    def last_snapshot(self) -> Optional[Dict[str, Any]]:
        with self._lock:
            return self._last_snapshot

    def restore(self) -> Dict[str, Any]:
        with self._lock:
            restored = self.snapshots.load_current()
            if not self._active:
                self._published = AnalysisState(**{
                    "schema_version": restored["state"]["schema_version"],
                    "config": restored["state"]["config"],
                    "manifest_digest": restored["state"]["manifest_digest"],
                    "geometry_digest": restored["state"]["geometry_digest"],
                    "alignments": tuple(restored["state"]["alignments"]),
                    "coupling": tuple(restored["state"]["coupling"]),
                    "route_matrix": tuple(restored["state"]["route_matrix"]),
                    "budget": tuple(restored["state"]["budget"]),
                    "phase_findings": tuple(restored["state"]["phase_findings"]),
                    "phase_summary": restored["state"]["phase_summary"],
                    "calibration": restored["state"]["calibration"],
                    "errors": tuple(restored["state"]["errors"]),
                    "incremental": restored["state"]["incremental"],
                    "coupling_diff": restored["state"]["coupling_diff"],
                    "node_hashes": restored["state"]["node_hashes"],
                    "trace_hash": restored["state"]["trace_hash"],
                    "status": restored["state"]["status"],
                    "content_hash_value": restored["metadata"]["state_hash"],
                })
            return restored
