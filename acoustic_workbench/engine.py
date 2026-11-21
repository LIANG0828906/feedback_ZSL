"""Acoustic reconstruction orchestration, invalidation and publication."""

from __future__ import annotations

import math
import os
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple

import numpy as np

from .cache import ContentCache, NodeResult
from .cancellation import CancelToken, checkpoint
from .codes import (
    AcousticError,
    E_BUSY,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_FIELD_STALE,
    E_IO,
    E_SYNTHESIS_FAILED,
)
from .dsp import (
    align_direct_sound,
    compute_field_coefficients,
    extract_early_reflections,
    model_decay,
    render_channels,
    synthesize_late_reverb,
    synthesize_spatial_response,
)
from .injection import (
    Injection,
    inject_ir_value,
    prepare_scene_injection,
    validate_ir_runtime,
)
from .locks import TaskLeaseManager
from .scene import (
    FACTOR_ORDER,
    STAGE_ORDER,
    clone_scene,
    earliest_affected_stage,
    factor_hashes,
    listener_by_id,
    load_scene,
    material_profile,
    stage_fingerprints,
    stages_from,
    validate_scene,
)
from .trajectory import TrajectoryLog
from .util import (
    array_bytes,
    as_jsonable,
    atomic_write_json,
    canonical_json_bytes,
    hash_obj,
    read_json,
    sha256_bytes,
    sha256_file,
    stable_seed,
)
from .wavio import WavData, read_wav_16, write_wav_16_atomic


def _reason_map(changed_factors: Sequence[str]) -> Dict[str, str]:
    return {
        factor: f"impact factor changed: {factor}"
        for factor in changed_factors
    }


def _ordered_unique(values: Iterable[str]) -> List[str]:
    seen = set()
    result: List[str] = []
    for value in values:
        if value not in seen:
            result.append(value)
            seen.add(value)
    return result


class Workbench:
    """One scene/listener reconstruction workspace."""

    def __init__(
        self,
        workspace: str | os.PathLike[str],
        scene_path: Optional[str | os.PathLike[str]] = None,
    ) -> None:
        self.workspace = Path(workspace)
        self.workspace.mkdir(parents=True, exist_ok=True)
        self.scene_path = self._resolve_scene_path(scene_path)
        self.cache = ContentCache(self.workspace / "cache")
        self.tasks = TaskLeaseManager(self.workspace / "state" / "locks")
        self.trajectory = TrajectoryLog(self.workspace / "logs" / "trajectory.jsonl")
        self.renders = self.workspace / "renders"
        self.renders.mkdir(parents=True, exist_ok=True)

    def _resolve_scene_path(self, scene_path: Optional[str | os.PathLike[str]]) -> Path:
        if scene_path is None:
            return self.workspace / "scene.json"
        candidate = Path(scene_path)
        if candidate.is_absolute():
            return candidate
        direct = self.workspace / candidate
        if direct.exists():
            return direct
        return candidate

    def use_scene(self, scene_path: str | os.PathLike[str]) -> None:
        self.scene_path = self._resolve_scene_path(scene_path)

    def scene(self) -> Dict[str, Any]:
        scene = load_scene(self.scene_path)
        scene["_scene_dir"] = str(self.scene_path.parent)
        return scene

    def listeners(self) -> List[str]:
        scene = self.scene()
        return [str(item["id"]) for item in scene.get("listeners", []) if item.get("id")]

    def _normalize_injections(
        self, injections: Optional[Sequence[Any]]
    ) -> List[Injection]:
        result: List[Injection] = []
        for item in injections or []:
            if isinstance(item, Injection):
                result.append(item)
            elif isinstance(item, str):
                result.append(Injection.parse(item))
            else:
                raise AcousticError(
                    E_CONFIG_INVALID,
                    "Injection must be an Injection or string.",
                    injection_point="request:inject",
                )
        return result

    def _prepare_input(
        self,
        listener_id: str,
        injections: Optional[Sequence[Any]],
    ) -> Tuple[Dict[str, Any], Dict[str, Any], WavData, List[Dict[str, Any]]]:
        raw_scene = self.scene()
        prepared = prepare_scene_injection(
            raw_scene,
            listener_id,
            self._normalize_injections(injections),
        )
        scene = prepared.scene
        scene["_scene_dir"] = str(self.scene_path.parent)
        try:
            validated = validate_scene(scene, listener_id)
        except AcousticError as exc:
            record = next(
                (
                    item
                    for item in prepared.records
                    if item.get("kind") in {
                        "decay_negative",
                        "position_out_of_bounds",
                        "channel_layout_missing",
                    }
                ),
                None,
            )
            if record is not None:
                exc.details = dict(exc.details)
                exc.details.setdefault("detected_at", exc.injection_point)
                exc.details["injection_kind"] = record.get("kind")
                exc.details["injected_at"] = record.get("injected_at")
                exc.injection_point = str(record.get("injected_at"))
            raise
        listener = listener_by_id(scene, listener_id)
        ir_path = Path(str(listener["ir"]))
        if not ir_path.is_absolute():
            ir_path = self.scene_path.parent / ir_path
        if not ir_path.is_file():
            raise AcousticError(
                E_IO,
                "Listener IR does not exist.",
                injection_point=f"input:listeners[{listener_id}].ir",
                details={"detected_at": "wav:path"},
            )
        wav = read_wav_16(ir_path)
        if prepared.ir_invalid:
            injected = inject_ir_value(wav.samples, prepared.records)
            wav.samples = injected["samples"]
            prepared.records = [
                injected.get("metadata", record)
                if record.get("kind") == "ir_invalid"
                else record
                for record in prepared.records
            ]
        validate_ir_runtime(
            wav.samples,
            sample_rate=wav.sample_rate,
            expected_rate=int(scene.get("sample_rate", 48000)),
            channel_count=len(validated["layout"]),
            layout=validated["layout"],
            injection_records=prepared.records,
        )
        wav.path = str(ir_path)
        return scene, validated, wav, prepared.records

    def _fingerprints(
        self,
        scene: Mapping[str, Any],
        listener_id: str,
        raw_wav_sha256: str,
    ) -> Dict[str, str]:
        return stage_fingerprints(
            scene,
            listener_id,
            raw_wav_sha256=raw_wav_sha256,
        )

    def _relative_scene_ir(self, listener: Mapping[str, Any]) -> str:
        path = Path(str(listener["ir"]))
        if path.is_absolute():
            try:
                return str(path.relative_to(self.scene_path.parent))
            except ValueError:
                return path.name
        return str(path)

    def _compute_stage(
        self,
        stage: str,
        *,
        scene: Mapping[str, Any],
        validated: Mapping[str, Any],
        listener: Mapping[str, Any],
        listener_id: str,
        wav: WavData,
        factors: Mapping[str, str],
        fingerprints: Mapping[str, str],
        prior: Mapping[str, NodeResult],
        cancel_token: Optional[CancelToken],
    ) -> Tuple[Dict[str, Any], Dict[str, np.ndarray], Dict[str, str], Dict[str, Any]]:
        layout = list(validated["layout"])
        source_position = list(validated["source_position"])
        listener_position = list(validated["listener_position"])
        distance = float(
            np.linalg.norm(
                np.asarray(listener_position, dtype=np.float64)
                - np.asarray(source_position, dtype=np.float64)
            )
        )

        if stage == "ir_load":
            summary = {
                "truth_source": {
                    "wav_sha256": wav.raw_sha256,
                    "sample_index_space": "original_wav_frame",
                    "sample_rate": wav.sample_rate,
                    "bits_per_sample": wav.bits_per_sample,
                    "channel_count": wav.channel_count,
                    "frame_count": wav.frame_count,
                    "channel_layout": layout,
                }
            }
            return summary, {"samples": wav.samples}, {"raw_wav": wav.raw_sha256}, {
                "layout": layout,
                "sample_rate": wav.sample_rate,
            }

        if stage == "direct_alignment":
            samples = prior["ir_load"].arrays["samples"]
            alignment = scene.get("alignment", {})
            result = align_direct_sound(
                samples,
                layout=layout,
                reference_channel=str(validated["reference_channel"]),
                threshold=float(alignment.get("threshold", 0.05)),
                normalize_gain=bool(alignment.get("normalize_gain", True)),
            )
            reference = int(result["reference_arrival"])
            channel_records = []
            for index, channel in enumerate(layout):
                channel_records.append(
                    {
                        "channel": channel,
                        "channel_index": index,
                        "source_sample_index": int(result["arrivals"][index]),
                        "aligned_sample_index": int(
                            result["arrivals"][index] - int(result["shift_samples"][index])
                        ),
                        "arrival_delta_samples": int(result["shift_samples"][index]),
                        "detected_peak": round(float(result["peaks"][index]), 12),
                        "gain": round(float(result["gains"][index]), 12),
                    }
                )
            summary = {
                "reference_channel": str(validated["reference_channel"]),
                "reference_sample_index": reference,
                "threshold": float(alignment.get("threshold", 0.05)),
                "normalize_gain": bool(alignment.get("normalize_gain", True)),
                "truth_source": {
                    "wav_sha256": wav.raw_sha256,
                    "wav_file": self._relative_scene_ir(listener),
                    "sample_index_space": "original_wav_frame",
                },
                "channels": channel_records,
            }
            return (
                summary,
                {
                    "aligned": np.asarray(result["aligned"], dtype=np.float64),
                    "arrival_samples": np.asarray(result["arrivals"], dtype=np.int64),
                    "gains": np.asarray(result["gains"], dtype=np.float64),
                },
                {"load": fingerprints["ir_load"]},
                {
                    "threshold": float(alignment.get("threshold", 0.05)),
                    "reference_channel": str(validated["reference_channel"]),
                    "normalize_gain": bool(alignment.get("normalize_gain", True)),
                },
            )

        if stage == "early_reflections":
            aligned = prior["direct_alignment"].arrays["aligned"]
            arrivals = prior["direct_alignment"].arrays["arrival_samples"]
            alignment_summary = prior["direct_alignment"].summary
            reference_index = list(layout).index(str(validated["reference_channel"]))
            result = extract_early_reflections(
                aligned,
                sample_rate=int(scene.get("sample_rate", 48000)),
                layout=layout,
                arrivals=arrivals,
                reference_index=reference_index,
                window=scene["window_params"],
                frequency=scene["frequency_config"],
                positions={
                    "source_position": source_position,
                    "listener_position": listener_position,
                    "distance_m": distance,
                },
                cancel_token=cancel_token,
            )
            summary = {
                "window_params": dict(scene["window_params"]),
                "frequency_config": {
                    "bands_hz": [float(item) for item in scene["frequency_config"]["bands_hz"]],
                    "fft_size": int(scene["frequency_config"]["fft_size"]),
                },
                "direct_end_sample": int(result["direct_end_sample"]),
                "early_end_sample": int(result["early_end_sample"]),
                "event_gate_absolute": float(result["event_gate_absolute"]),
                "event_count": len(result["events"]),
                "events": result["events"],
                "positions": result["positions"],
                "reference_sample_index": int(alignment_summary["reference_sample_index"]),
            }
            return (
                summary,
                {"early_ir": np.asarray(result["early_ir"], dtype=np.float64)},
                {"alignment": fingerprints["direct_alignment"]},
                {
                    "window_params": dict(scene["window_params"]),
                    "frequency_config": dict(scene["frequency_config"]),
                    "source_position": source_position,
                    "listener_position": listener_position,
                },
            )

        if stage == "decay_model":
            early = prior["early_reflections"]
            decay_config = scene.get("decay_config", {})
            result = model_decay(
                early.arrays["early_ir"],
                sample_rate=int(scene.get("sample_rate", 48000)),
                reference_sample=int(early.summary["reference_sample_index"]),
                early_end_sample=int(early.summary["early_end_sample"]),
                frequency=scene["frequency_config"],
                material=material_profile(scene),
                decay_config=decay_config,
                source_position=source_position,
                listener_position=listener_position,
                cancel_token=cancel_token,
            )
            summary = {
                "bands_hz": list(result["bands_hz"]),
                "frame_count": int(result["frame_times_sec"].size),
                "decay_times_sec": [
                    round(float(item), 12) for item in result["decay_times_sec"]
                ],
                "fit_slopes_db_per_sec": [
                    round(float(item), 12) for item in result["fit_slopes_db_per_sec"]
                ],
                "material": result["material"],
                "distance_m": round(float(result["distance_m"]), 12),
                "override_used": bool(result["override_used"]),
            }
            return (
                summary,
                {
                    "frame_times_sec": np.asarray(result["frame_times_sec"], dtype=np.float64),
                    "band_energy_db": np.asarray(result["band_energy_db"], dtype=np.float64),
                    "schroeder_db": np.asarray(result["schroeder_db"], dtype=np.float64),
                    "decay_times_sec": np.asarray(result["decay_times_sec"], dtype=np.float64),
                    "fit_slopes_db_per_sec": np.asarray(
                        result["fit_slopes_db_per_sec"], dtype=np.float64
                    ),
                },
                {"early_reflections": fingerprints["early_reflections"]},
                {
                    "frequency_config": dict(scene["frequency_config"]),
                    "material": material_profile(scene),
                    "decay_config": dict(decay_config),
                    "source_position": source_position,
                    "listener_position": listener_position,
                },
            )

        if stage == "late_reverb":
            decay = prior["decay_model"]
            early = prior["early_reflections"]
            reverb = scene.get("reverb", {})
            seed_base = int(reverb.get("seed", 2026))
            seed = stable_seed(f"{seed_base}:{fingerprints['late_reverb']}")
            result = synthesize_late_reverb(
                decay_times=decay.arrays["decay_times_sec"],
                bands_hz=decay.summary["bands_hz"],
                sample_rate=int(scene.get("sample_rate", 48000)),
                tail_ms=float(scene["window_params"]["tail_ms"]),
                early_ir=early.arrays["early_ir"],
                early_end_sample=int(early.summary["early_end_sample"]),
                seed=seed,
                material=decay.summary["material"],
                source_position=source_position,
                listener_position=listener_position,
                cancel_token=cancel_token,
            )
            summary = {
                "tail_samples": int(result["tail_samples"]),
                "seed": seed,
                "tail_ms": float(scene["window_params"]["tail_ms"]),
            }
            return (
                summary,
                {"late_tail": np.asarray(result["late_tail"], dtype=np.float64)},
                {
                    "decay_model": fingerprints["decay_model"],
                    "early_reflections": fingerprints["early_reflections"],
                },
                {
                    "reverb_config": dict(scene.get("reverb", {})),
                    "tail_ms": float(scene["window_params"]["tail_ms"]),
                    "seed": seed,
                    "layout": layout,
                },
            )

        if stage == "spatial_synthesis":
            early = prior["early_reflections"]
            reverb = prior["late_reverb"]
            align = prior["direct_alignment"]
            result = synthesize_spatial_response(
                early.arrays["early_ir"],
                reverb.arrays["late_tail"],
                early_end_sample=int(early.summary["early_end_sample"]),
                layout=layout,
                align_arrivals=align.arrays["arrival_samples"],
                cancel_token=cancel_token,
            )
            summary = {
                "early_end_sample": int(result["early_end_sample"]),
                "frame_count": int(result["spatial_ir"].shape[0]),
                "channel_layout": layout,
                "channel_alignment_error_samples": result["alignment_error_samples"],
            }
            return (
                summary,
                {"spatial_ir": np.asarray(result["spatial_ir"], dtype=np.float64)},
                {
                    "late_reverb": fingerprints["late_reverb"],
                    "early_reflections": fingerprints["early_reflections"],
                    "direct_alignment": fingerprints["direct_alignment"],
                },
                {"layout": layout, "sample_rate": int(scene.get("sample_rate", 48000))},
            )

        if stage == "field_coefficients":
            spatial = prior["spatial_synthesis"]
            decay = prior["decay_model"]
            align = prior["direct_alignment"]
            result = compute_field_coefficients(
                spatial.arrays["spatial_ir"],
                sample_rate=int(scene.get("sample_rate", 48000)),
                layout=layout,
                decay_times=decay.arrays["decay_times_sec"],
                bands_hz=decay.summary["bands_hz"],
                arrivals=align.arrays["arrival_samples"],
                early_end_sample=int(spatial.summary["early_end_sample"]),
                source_position=source_position,
                listener_position=listener_position,
                fft_size=int(scene["frequency_config"]["fft_size"]),
                cancel_token=cancel_token,
            )
            filters = np.asarray(result["filters"], dtype=np.float32)
            filter_hash = sha256_bytes(array_bytes(filters))
            summary = {
                "coefficients": result["coefficients"],
                "filter_blob": {
                    "hash": filter_hash,
                    "shape": [int(item) for item in filters.shape],
                    "dtype": str(filters.dtype),
                },
                "channel_layout": layout,
                "sample_rate": int(scene.get("sample_rate", 48000)),
                "azimuth_degrees": round(float(result["azimuth_degrees"]), 9),
            }
            return (
                summary,
                {"filters": filters},
                {
                    "spatial_synthesis": fingerprints["spatial_synthesis"],
                    "decay_model": fingerprints["decay_model"],
                },
                {
                    "layout": layout,
                    "source_position": source_position,
                    "listener_position": listener_position,
                },
            )
        raise AcousticError(
            E_CONFIG_INVALID,
            f"Unknown pipeline stage: {stage}",
            injection_point="pipeline:stage",
        )

    def _compute_stage_checked(
        self,
        stage: str,
        **kwargs: Any,
    ) -> Tuple[Dict[str, Any], Dict[str, np.ndarray], Dict[str, str], Dict[str, Any]]:
        try:
            return self._compute_stage(stage, **kwargs)
        except AcousticError:
            raise
        except Exception as exc:
            raise AcousticError(
                E_SYNTHESIS_FAILED,
                f"Pipeline stage {stage} failed before any field publication.",
                injection_point=f"pipeline:{stage}",
                details={"error_type": type(exc).__name__},
            ) from exc

    def _node_result(
        self,
        *,
        stage: str,
        fingerprint: str,
        inputs: Mapping[str, str],
        params: Mapping[str, Any],
        summary: Mapping[str, Any],
        arrays: Mapping[str, np.ndarray],
    ) -> NodeResult:
        return self.cache.put_node(
            stage=stage,
            fingerprint=fingerprint,
            inputs=inputs,
            params=params,
            summary=as_jsonable(summary),
            arrays=arrays,
        )

    def _field_payload(
        self,
        *,
        scene: Mapping[str, Any],
        listener_id: str,
        factors: Mapping[str, str],
        fingerprints: Mapping[str, str],
        field_result: NodeResult,
    ) -> Dict[str, Any]:
        field_summary = field_result.summary
        field_meta = self.cache.get_node_meta(
            "field_coefficients", fingerprints["field_coefficients"]
        )
        if field_meta is None:
            raise AcousticError(
                E_SYNTHESIS_FAILED,
                "Field coefficient cache metadata is missing.",
                injection_point="cache:field_coefficients",
            )
        payload = {
            "schema_version": 1,
            "scene_id": scene["scene_id"],
            "venue_id": scene["venue"].get("id", ""),
            "listener_id": listener_id,
            "sample_rate": int(scene.get("sample_rate", 48000)),
            "channel_layout": list(scene["channel_layout"]),
            "dependency_manifest": dict(fingerprints),
            "factor_hashes": dict(factors),
            "coefficients": field_summary["coefficients"],
            "filter_blob": field_summary["filter_blob"],
            "field_node_hash": field_result.result_hash,
            "field_node_arrays": field_meta["arrays"],
        }
        field_version = hash_obj(payload)
        payload["field_version"] = field_version
        return payload

    def _pub_diff(
        self,
        previous_state: Optional[Mapping[str, Any]],
        current_fingerprints: Mapping[str, str],
        *,
        changed_factors: Sequence[str],
        prior_results: Mapping[str, NodeResult],
        current_results: Mapping[str, NodeResult],
    ) -> Dict[str, Any]:
        output: Dict[str, Any] = {
            "arrival_sample_differences": None,
            "reflection_event_impact": None,
        }
        if not previous_state:
            return output
        previous_nodes = previous_state.get("node_fingerprints", {})
        if (
            "alignment" in changed_factors
            or previous_nodes.get("direct_alignment")
            != current_fingerprints.get("direct_alignment")
        ):
            old_meta = self.cache.get_node_meta(
                "direct_alignment", previous_nodes.get("direct_alignment", "")
            )
            new_meta = self.cache.get_node_meta(
                "direct_alignment", current_fingerprints.get("direct_alignment", "")
            )
            if old_meta and new_meta:
                old_channels = {
                    item["channel"]: item
                    for item in old_meta["summary"].get("channels", [])
                }
                differences = []
                for item in new_meta["summary"].get("channels", []):
                    old = old_channels.get(item["channel"], {})
                    differences.append(
                        {
                            "channel": item["channel"],
                            "old_source_sample_index": old.get("source_sample_index"),
                            "new_source_sample_index": item.get("source_sample_index"),
                            "source_delta_samples": (
                                item.get("source_sample_index", 0)
                                - old.get("source_sample_index", 0)
                                if old
                                else None
                            ),
                            "old_aligned_sample_index": old.get("aligned_sample_index"),
                            "new_aligned_sample_index": item.get("aligned_sample_index"),
                            "aligned_delta_samples": (
                                item.get("aligned_sample_index", 0)
                                - old.get("aligned_sample_index", 0)
                                if old
                                else None
                            ),
                        }
                    )
                output["arrival_sample_differences"] = differences

        early_changed = any(
            factor
            in {
                "window_params",
                "frequency_config",
                "source_position",
                "listener_position",
            }
            for factor in changed_factors
        ) or previous_nodes.get("early_reflections") != current_fingerprints.get(
            "early_reflections"
        )
        decay_changed = any(
            factor in {"material", "decay_config"} for factor in changed_factors
        ) or previous_nodes.get("decay_model") != current_fingerprints.get("decay_model")
        if early_changed or decay_changed:
            old_meta = self.cache.get_node_meta(
                "early_reflections", previous_nodes.get("early_reflections", "")
            )
            new_meta = self.cache.get_node_meta(
                "early_reflections", current_fingerprints.get("early_reflections", "")
            )
            old_events = old_meta["summary"].get("events", []) if old_meta else []
            new_events = new_meta["summary"].get("events", []) if new_meta else []
            old_by_key = {
                (event.get("channel"), event.get("event_index")): event
                for event in old_events
            }
            changed_indices = []
            for event in new_events:
                key = (event.get("channel"), event.get("event_index"))
                if canonical_json_bytes(old_by_key.get(key)) != canonical_json_bytes(event):
                    changed_indices.append(event.get("order"))
            old_keys = set(old_by_key)
            new_keys = {
                (event.get("channel"), event.get("event_index"))
                for event in new_events
            }
            removed = [
                {
                    "channel": key[0],
                    "event_index": key[1],
                }
                for key in sorted(old_keys - new_keys)
            ]
            added = [
                {
                    "channel": event.get("channel"),
                    "event_index": event.get("event_index"),
                    "order": event.get("order"),
                }
                for event in new_events
                if (event.get("channel"), event.get("event_index"))
                not in old_keys
            ]
            scope_all = early_changed
            affected = (
                list(range(len(new_events))) if scope_all else changed_indices
            )
            output["reflection_event_impact"] = {
                "recomputed": early_changed
                or previous_nodes.get("early_reflections")
                != current_fingerprints.get("early_reflections"),
                "affected_event_indices": affected,
                "changed_event_indices": changed_indices,
                "added_events": added,
                "removed_events": removed,
                "reason": sorted(
                    set(changed_factors)
                    | (
                        {"decay_model_parameters"}
                        if decay_changed and not early_changed
                        else set()
                    )
                ),
            }
        return output

    def rebuild(
        self,
        listener_id: str,
        *,
        injections: Optional[Sequence[Any]] = None,
        cancel_token: Optional[CancelToken] = None,
        cancel_after_stage: Optional[str] = None,
    ) -> Dict[str, Any]:
        with self.tasks.acquire(listener_id, "rebuild"):
            try:
                return self._rebuild_locked(
                    listener_id,
                    injections=injections,
                    cancel_token=cancel_token,
                    cancel_after_stage=cancel_after_stage,
                )
            except AcousticError as exc:
                event = {
                    "operation": "rebuild",
                    "status": "cancelled" if exc.code == E_CANCELLED else "failed",
                    "listener_id": listener_id,
                    "code": exc.code,
                    "injection_point": exc.injection_point,
                    "details": exc.details,
                }
                try:
                    self.trajectory.append(event)
                except OSError:
                    pass
                raise

    def _rebuild_locked(
        self,
        listener_id: str,
        *,
        injections: Optional[Sequence[Any]],
        cancel_token: Optional[CancelToken],
        cancel_after_stage: Optional[str],
    ) -> Dict[str, Any]:
        scene, validated, wav, injection_records = self._prepare_input(
            listener_id, injections
        )
        listener = listener_by_id(scene, listener_id)
        checkpoint(cancel_token, "rebuild:input_validated")
        factors = factor_hashes(scene, listener_id)
        fingerprints = self._fingerprints(scene, listener_id, wav.raw_sha256)
        previous_state = self.cache.read_state(listener_id)
        previous_factors = (
            previous_state.get("factor_hashes", {}) if previous_state else {}
        )
        changed_factors = [
            factor
            for factor in FACTOR_ORDER
            if not previous_state or previous_factors.get(factor) != factors.get(factor)
        ]
        changed_reasons = _reason_map(changed_factors)
        if previous_state and previous_state.get("raw_wav_sha256") != wav.raw_sha256:
            changed_factors.append("ir_content")
            changed_reasons["ir_content"] = "raw WAV content hash changed"

        earliest = earliest_affected_stage(changed_factors)
        previous_nodes = (
            previous_state.get("node_fingerprints", {}) if previous_state else {}
        )
        for stage in STAGE_ORDER:
            if previous_state and previous_nodes.get(stage) != fingerprints[stage]:
                stage_index = STAGE_ORDER.index(stage)
                current_index = (
                    STAGE_ORDER.index(earliest)
                    if earliest is not None
                    else len(STAGE_ORDER)
                )
                if stage_index < current_index:
                    earliest = stage
                changed_reasons.setdefault(
                    f"stage:{stage}", "stage fingerprint differs from last commit"
                )
        invalidated_nodes = stages_from(earliest)
        invalidated_set = set(invalidated_nodes)

        results: Dict[str, NodeResult] = {}
        cache_hits: List[str] = []
        recomputed: List[str] = []
        conservative_recomputes: Dict[str, str] = {}
        checkpoints: List[str] = []
        for stage in STAGE_ORDER:
            checkpoint(cancel_token, f"stage:enter:{stage}")
            cached = self.cache.get_node(stage, fingerprints[stage])
            if cached is not None:
                results[stage] = cached
                cache_hits.append(stage)
            else:
                if stage not in invalidated_set:
                    conservative_recomputes[stage] = (
                        "cache miss for an otherwise unaffected node; recomputed conservatively"
                    )
                summary, arrays, inputs, params = self._compute_stage_checked(
                    stage,
                    scene=scene,
                    validated=validated,
                    listener=listener,
                    listener_id=listener_id,
                    wav=wav,
                    factors=factors,
                    fingerprints=fingerprints,
                    prior=results,
                    cancel_token=cancel_token,
                )
                result = self._node_result(
                    stage=stage,
                    fingerprint=fingerprints[stage],
                    inputs=inputs,
                    params=params,
                    summary=summary,
                    arrays=arrays,
                )
                results[stage] = result
                recomputed.append(stage)
                checkpoints.append(stage)
            if cancel_after_stage == stage:
                raise AcousticError(
                    E_CANCELLED,
                    "Task cancelled after the requested deterministic checkpoint.",
                    injection_point=f"checkpoint:after:{stage}",
                )
            checkpoint(cancel_token, f"stage:after:{stage}")

        field_result = results["field_coefficients"]
        field_document = self._field_payload(
            scene=scene,
            listener_id=listener_id,
            factors=factors,
            fingerprints=fingerprints,
            field_result=field_result,
        )
        checkpoint(cancel_token, "publish:before")
        field_version = self.cache.publish_field(listener_id, field_document)
        checkpoint(cancel_token, "publish:after")
        state = {
            "schema_version": 1,
            "listener_id": listener_id,
            "scene_id": scene["scene_id"],
            "raw_wav_sha256": wav.raw_sha256,
            "factor_hashes": factors,
            "node_fingerprints": fingerprints,
            "field_version": field_version,
            "injection_records": injection_records,
        }
        self.cache.write_state(listener_id, state)

        previous_results: Dict[str, NodeResult] = {}
        if previous_state:
            for stage in ("direct_alignment", "early_reflections"):
                fp = previous_nodes.get(stage)
                if fp:
                    previous = self.cache.get_node(stage, fp)
                    if previous:
                        previous_results[stage] = previous
        diff = self._pub_diff(
            previous_state,
            fingerprints,
            changed_factors=changed_factors,
            prior_results=previous_results,
            current_results=results,
        )

        preserved = [stage for stage in STAGE_ORDER if stage not in recomputed]
        event = {
            "operation": "rebuild",
            "status": "ok",
            "listener_id": listener_id,
            "scene_id": scene["scene_id"],
            "factors_changed": changed_factors,
            "factor_change_reasons": changed_reasons,
            "earliest_affected_node": earliest,
            "invalidated_nodes": invalidated_nodes,
            "recomputed_nodes": recomputed,
            "cache_hits": cache_hits,
            "preserved_nodes": preserved,
            "conservative_recomputes": conservative_recomputes,
            "checkpoints_completed": checkpoints,
            "node_fingerprints": fingerprints,
            "node_result_hashes": {
                stage: results[stage].result_hash for stage in STAGE_ORDER
            },
            "raw_wav_sha256": wav.raw_sha256,
            "field_version": field_version,
            "filter_sha256": field_document["filter_blob"]["hash"],
            "channel_layout": list(scene["channel_layout"]),
            **diff,
        }
        self.trajectory.append(as_jsonable(event))
        return {
            "ok": True,
            "field_version": field_version,
            "field": field_document,
            "trajectory": event,
        }

    def _verify_field(
        self,
        listener_id: str,
        expected_manifest: Mapping[str, str],
    ) -> Dict[str, Any]:
        field = self.cache.read_published(listener_id)
        if not field:
            raise AcousticError(
                E_FIELD_STALE,
                "No published field coefficient version exists for this listener.",
                injection_point=f"render:field:{listener_id}",
                details={"detected_at": "render:version_manifest", "listener_id": listener_id},
            )
        payload = dict(field)
        stored_version = payload.pop("field_version", None)
        computed_version = hash_obj(payload)
        if not stored_version or stored_version != computed_version:
            raise AcousticError(
                E_FIELD_STALE,
                "Published field content hash does not match its version manifest.",
                injection_point=f"render:field:{listener_id}:field_version",
                details={
                    "detected_at": "render:version_manifest",
                    "stored_field_version": stored_version,
                    "computed_field_version": computed_version,
                    "field_version": stored_version,
                },
            )
        stored_manifest = field.get("dependency_manifest", {})
        mismatched = [
            stage
            for stage in STAGE_ORDER
            if stored_manifest.get(stage) != expected_manifest.get(stage)
        ]
        if mismatched:
            raise AcousticError(
                E_FIELD_STALE,
                "Published field coefficients were built from stale dependencies.",
                injection_point=f"render:field:{listener_id}:dependency_manifest",
                details={
                    "detected_at": "render:version_manifest",
                    "field_version": stored_version,
                    "stale_stages": mismatched,
                    "stored_manifest": stored_manifest,
                    "expected_manifest": dict(expected_manifest),
                },
            )
        return field

    def render(
        self,
        listener_id: str,
        *,
        output: Optional[str | os.PathLike[str]] = None,
        injections: Optional[Sequence[Any]] = None,
        cancel_token: Optional[CancelToken] = None,
    ) -> Dict[str, Any]:
        with self.tasks.acquire(listener_id, "render"):
            try:
                return self._render_locked(
                    listener_id,
                    output=output,
                    injections=injections,
                    cancel_token=cancel_token,
                )
            except AcousticError as exc:
                event = {
                    "operation": "render",
                    "status": "cancelled" if exc.code == E_CANCELLED else "failed",
                    "listener_id": listener_id,
                    "code": exc.code,
                    "injection_point": exc.injection_point,
                    "details": exc.details,
                }
                try:
                    self.trajectory.append(as_jsonable(event))
                except OSError:
                    pass
                raise

    def _render_locked(
        self,
        listener_id: str,
        *,
        output: Optional[str | os.PathLike[str]],
        injections: Optional[Sequence[Any]],
        cancel_token: Optional[CancelToken],
    ) -> Dict[str, Any]:
        scene, validated, wav, injection_records = self._prepare_input(
            listener_id, injections
        )
        checkpoint(cancel_token, "render:input_validated")
        expected_manifest = self._fingerprints(scene, listener_id, wav.raw_sha256)
        field = self._verify_field(listener_id, expected_manifest)
        filter_ref = field.get("filter_blob", {})
        filter_hash = filter_ref.get("hash")
        try:
            filters = self.cache.get_blob(str(filter_hash))
        except (OSError, ValueError) as exc:
            raise AcousticError(
                E_FIELD_STALE,
                "Published field filter blob is missing or corrupt.",
                injection_point=f"render:field:{listener_id}:filter_blob",
                details={"detected_at": "render:filter_blob", "filter_hash": filter_hash},
            ) from exc
        if list(filters.shape) != list(filter_ref.get("shape", [])):
            raise AcousticError(
                E_FIELD_STALE,
                "Published field filter shape does not match its manifest.",
                injection_point=f"render:field:{listener_id}:filter_shape",
            )
        try:
            rendered = render_channels(
                filters,
                sample_rate=int(field["sample_rate"]),
                layout=list(field["channel_layout"]),
                render_config=scene.get("render", {}),
                cancel_token=cancel_token,
            )
        except AcousticError:
            raise
        except Exception as exc:
            raise AcousticError(
                E_SYNTHESIS_FAILED,
                "Multi-channel rendering failed; no output was written.",
                injection_point="render:channels",
                details={"error_type": type(exc).__name__},
            ) from exc
        checkpoint(cancel_token, "render:before_write")
        if output is None:
            target = self.renders / f"{listener_id}.wav"
        else:
            candidate = Path(output)
            target = candidate if candidate.is_absolute() else self.workspace / candidate
        try:
            output_sha256 = write_wav_16_atomic(
                target,
                rendered["samples"],
                int(field["sample_rate"]),
                0x3F,
            )
        except (OSError, ValueError) as exc:
            raise AcousticError(
                E_IO,
                f"Cannot write rendered WAV: {exc}",
                injection_point="render:output",
            ) from exc
        try:
            relative_output = str(target.relative_to(self.workspace))
        except ValueError:
            relative_output = target.name
        event = {
            "operation": "render",
            "status": "ok",
            "listener_id": listener_id,
            "scene_id": scene["scene_id"],
            "field_version": field["field_version"],
            "dependency_manifest": dict(expected_manifest),
            "channel_layout": list(field["channel_layout"]),
            "output": relative_output,
            "output_sha256": output_sha256,
            "filter_sha256": filter_hash,
            "peak_dbfs": rendered["peak_dbfs"],
            "injection_records": injection_records,
        }
        self.trajectory.append(as_jsonable(event))
        return {
            "ok": True,
            "output": str(target),
            "output_sha256": output_sha256,
            "field_version": field["field_version"],
            "trajectory": event,
        }

    def status(self) -> Dict[str, Any]:
        scene = self.scene()
        listeners = []
        for listener in sorted(scene.get("listeners", []), key=lambda item: item.get("id", "")):
            listener_id = str(listener["id"])
            field = self.cache.read_published(listener_id)
            listeners.append(
                {
                    "listener_id": listener_id,
                    "field_version": field.get("field_version") if field else None,
                    "dependency_manifest": field.get("dependency_manifest") if field else None,
                }
            )
        return {
            "scene_id": scene.get("scene_id"),
            "scene_path": self.scene_path.name,
            "listeners": listeners,
        }

