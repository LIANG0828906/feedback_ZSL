from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .alignment import align_impulse_responses
from .cache import CacheStore, NodeResult
from .cancellation import CancellationToken
from .decay import model_band_decay
from .errors import (
    E_CONFIG_INVALID,
    E_FIELD_STALE,
    E_WAV_INVALID,
    fail,
)
from .field import field_manifest
from .geometry import (
    material_signature,
    reflection_paths,
)
from .graph import NODE_ORDER, invalidation_plan
from .hashing import canonical_json, sha256_array, sha256_file, sha256_json
from .late import synthesize_late_reverb
from .locks import PointLocks
from .model import Scene, load_materials, normalize_factor_name
from .reflections import extract_early_reflections
from .render import render_field
from .stitch import compute_field_coefficients, stitch_multichannel_response
from .wavio import read_wav


NODE_IMPL_VERSION = "acoustic-nodes/2026-11-20/v1"


@dataclass
class NodeTrace:
    node: str
    status: str
    cache_key: str
    output_hash: str
    reason: list[str]

    def to_dict(self) -> dict[str, Any]:
        return {
            "node": self.node,
            "status": self.status,
            "cache_key": self.cache_key,
            "output_hash": self.output_hash,
            "reason": list(self.reason),
        }


@dataclass
class PointReport:
    point_id: str
    invalidated_nodes: list[str] = field(default_factory=list)
    reasons: dict[str, list[str]] = field(default_factory=dict)
    nodes: list[NodeTrace] = field(default_factory=list)
    affected_reflection_events: list[str] = field(default_factory=list)
    field_version: str = ""
    coefficient_hash: str = ""
    response_hash: str = ""
    coefficient_path: str = ""
    field_manifest: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        recomputed = [item.node for item in self.nodes if item.status == "recomputed"]
        hits = [item.node for item in self.nodes if item.status == "cache_hit"]
        return {
            "point_id": self.point_id,
            "invalidated_nodes": list(self.invalidated_nodes),
            "invalidation_reasons": {
                key: list(value) for key, value in sorted(self.reasons.items())
            },
            "affected_reflection_events": list(self.affected_reflection_events),
            "nodes": [item.to_dict() for item in self.nodes],
            "recomputed_nodes": recomputed,
            "cache_hit_nodes": hits,
            "field_version": self.field_version,
            "coefficient_hash": self.coefficient_hash,
            "response_hash": self.response_hash,
            "coefficient_path": self.coefficient_path,
            "field_manifest": self.field_manifest,
        }


@dataclass
class RebuildReport:
    mode: str
    scene_id: str
    factors: list[dict[str, Any]] = field(default_factory=list)
    points: list[PointReport] = field(default_factory=list)

    def _union(self, attribute: str) -> list[str]:
        values: list[str] = []
        for point in self.points:
            for item in point.to_dict()[attribute]:
                if item not in values:
                    values.append(item)
        return values

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "scene_id": self.scene_id,
            "factors": self.factors,
            "invalidated_nodes": self._union("invalidated_nodes"),
            "recomputed_nodes": self._union("recomputed_nodes"),
            "cache_hit_nodes": self._union("cache_hit_nodes"),
            "points": [point.to_dict() for point in self.points],
        }


class AcousticEngine:
    def __init__(
        self,
        sample_dir: str | Path,
        *,
        cache_dir: str | Path | None = None,
        scene: Scene | None = None,
        materials: dict[str, Any] | None = None,
    ):
        self.sample_dir = Path(sample_dir)
        self.scene_path = self.sample_dir / "scene.json"
        self.scene = scene if scene is not None else Scene.load(self.scene_path)
        if materials is None:
            materials = load_materials(self.sample_dir / self.scene.materials_file)
        self.materials = materials
        self.cache = CacheStore(cache_dir if cache_dir is not None else self.sample_dir)
        self.locks = PointLocks(self.cache.lock_root)

    def _raw_digest(self, scene: Scene, listener_id: str) -> str:
        path = scene.resolved_ir_path(self.sample_dir, listener_id)
        if not path.is_file():
            fail(
                E_WAV_INVALID,
                "impulse response file is missing",
                listener_id=listener_id,
                path=str(path),
            )
        return sha256_file(path)

    def _cache_key(
        self,
        node: str,
        point_id: str,
        upstream: dict[str, str],
        effective: dict[str, Any],
    ) -> str:
        return sha256_json(
            {
                "schema": "acoustic-node-key/v1",
                "implementation": NODE_IMPL_VERSION,
                "node": node,
                "point": point_id,
                "upstream": dict(sorted(upstream.items())),
                "effective": effective,
            }
        )

    def _effective(
        self,
        scene: Scene,
        point_id: str,
        node: str,
    ) -> dict[str, Any]:
        listener = scene.listener(point_id)
        source = scene.source
        if node == "direct_align":
            return {
                "raw_sha256": self._raw_digest(scene, point_id),
                "sample_rate": scene.sample_rate,
                "channel_layout": scene.channel_layout,
                "reference_channel": scene.reference_channel,
                "source": [source.x, source.y, source.z],
                "listener": [listener.x, listener.y, listener.z],
                "alignment": scene.alignment,
            }
        if node == "early_reflections":
            centers = [
                float(value)
                for value in scene.bands.get("center_frequencies", [])
            ]
            return {
                "early": scene.early,
                "bands": scene.bands,
                "material_signature": material_signature(
                    scene, self.materials, centers
                ),
                "source": [source.x, source.y, source.z],
                "listener": [listener.x, listener.y, listener.z],
                "reflection_paths": reflection_paths(scene, point_id),
            }
        if node == "band_decay":
            return {
                "bands": scene.bands,
                "min_t60_s": scene.late.get("min_t60_s"),
                "max_t60_s": scene.late.get("max_t60_s"),
            }
        if node == "late_reverb":
            return {
                "early_end_ms": scene.early.get("end_ms"),
                "late": scene.late,
                "channel_layout": scene.channel_layout,
            }
        if node == "multichannel_stitch":
            return {"channel_layout": scene.channel_layout}
        if node == "field_coefficients":
            return {
                "channel_layout": scene.channel_layout,
                "bands": scene.bands,
                "reference_channel": scene.reference_channel,
            }
        fail(E_CONFIG_INVALID, f"unknown graph node: {node}")

    def _upstream_for(self, node: str, results: dict[str, NodeResult]) -> dict[str, str]:
        mapping = {
            "direct_align": [],
            "early_reflections": ["direct_align"],
            "band_decay": ["early_reflections"],
            "late_reverb": ["band_decay"],
            "multichannel_stitch": ["early_reflections", "late_reverb"],
            "field_coefficients": [
                "direct_align",
                "early_reflections",
                "band_decay",
                "late_reverb",
                "multichannel_stitch",
            ],
        }
        return {
            name: results[name].output_hash
            for name in mapping[node]
            if name in results
        }

    def _execute_node(
        self,
        scene: Scene,
        point_id: str,
        node: str,
        results: dict[str, NodeResult],
        field_key: str | None = None,
    ) -> tuple[dict[str, Any], dict[str, np.ndarray | bytes | str]]:
        if node == "direct_align":
            wav = read_wav(
                scene.resolved_ir_path(self.sample_dir, point_id),
                expected_channels=6,
            )
            if wav.sample_rate != scene.sample_rate:
                fail(
                    E_WAV_INVALID,
                    "impulse response sample rate must be 48000 Hz",
                    listener_id=point_id,
                    sample_rate=wav.sample_rate,
                )
            aligned = align_impulse_responses(wav.samples, scene, point_id)
            data = {
                "listener_id": point_id,
                "aligned_hash": sha256_array(aligned.aligned),
                "channels": len(scene.channel_layout),
                "frames": int(aligned.aligned.shape[1]),
                **aligned.metadata(),
            }
            return data, {"aligned.npy": aligned.aligned}

        if node == "early_reflections":
            aligned = np.load(results["direct_align"].artifact_path("aligned.npy"))
            segment, events, metadata = extract_early_reflections(
                aligned,
                scene,
                self.materials,
                point_id,
            )
            data = {
                **metadata,
                "event_count": len(events),
                "segment_hash": sha256_array(segment),
                "segment_frames": int(segment.shape[1]),
            }
            return data, {
                "early_segment.npy": segment,
                "events.json": events,
            }

        if node == "band_decay":
            events = list(results["early_reflections"].data.get("events", []))
            decay = model_band_decay(events, scene, point_id)
            return decay, {"decay.json": decay}

        if node == "late_reverb":
            decay = dict(results["band_decay"].data)
            late = synthesize_late_reverb(
                decay,
                scene,
                point_id,
                results["band_decay"].output_hash,
            )
            return (
                {
                    "listener_id": point_id,
                    "late_hash": sha256_array(late),
                    "tail_samples": int(
                        round(
                            float(scene.late.get("tail_ms", 800.0))
                            * scene.sample_rate
                            / 1000.0
                        )
                    ),
                    "early_end_sample": int(
                        round(
                            float(scene.early["end_ms"])
                            * scene.sample_rate
                            / 1000.0
                        )
                    ),
                    "frames": int(late.shape[1]),
                },
                {"late_response.npy": late},
            )

        if node == "multichannel_stitch":
            early_segment = np.load(
                results["early_reflections"].artifact_path("early_segment.npy")
            )
            late_response = np.load(
                results["late_reverb"].artifact_path("late_response.npy")
            )
            response = stitch_multichannel_response(
                early_segment,
                late_response,
                scene,
                point_id,
            )
            return (
                {
                    "listener_id": point_id,
                    "response_hash": sha256_array(response),
                    "channels": int(response.shape[0]),
                    "frames": int(response.shape[1]),
                    "channel_layout": list(scene.channel_layout),
                },
                {"response.npy": response},
            )

        if node == "field_coefficients":
            response = np.load(
                results["multichannel_stitch"].artifact_path("response.npy")
            )
            events = list(results["early_reflections"].data.get("events", []))
            summary = compute_field_coefficients(
                response,
                scene,
                results["band_decay"].data,
                point_id,
                events,
            )
            version = field_key or ""
            dependencies = []
            for name in NODE_ORDER[:-1]:
                result = results[name]
                primary_artifact = sorted(result.artifacts)[0]
                dependencies.append(
                    {
                        "node": name,
                        "output_hash": result.output_hash,
                        "artifact": primary_artifact,
                        "artifact_hash": result.artifacts[primary_artifact]["sha256"],
                    }
                )
            manifest = field_manifest(
                point_id=point_id,
                version=version,
                dependency_hash=version,
                dependencies=dependencies,
                coefficient_hash=sha256_array(response),
                summary=summary,
                channel_layout=scene.channel_layout,
            )
            return (
                {
                    "listener_id": point_id,
                    "field_manifest": manifest,
                    "field_version": version,
                    "coefficient_hash": manifest["coefficient_hash"],
                    "response_hash": results["multichannel_stitch"].data[
                        "response_hash"
                    ],
                },
                {
                    "coefficients.npy": response,
                    "field.json": manifest,
                },
            )

        fail(E_CONFIG_INVALID, f"unknown graph node: {node}")

    def _run_node(
        self,
        scene: Scene,
        point_id: str,
        node: str,
        results: dict[str, NodeResult],
        invalidated: list[str],
        token: CancellationToken | None,
    ) -> tuple[NodeResult, NodeTrace]:
        if token is not None:
            token.checkpoint(f"before:{point_id}:{node}")
        upstream = self._upstream_for(node, results)
        effective = self._effective(scene, point_id, node)
        key = self._cache_key(node, point_id, upstream, effective)
        cached = self.cache.load_node(point_id, node, key)
        if cached is not None:
            return cached, NodeTrace(
                node=node,
                status="cache_hit",
                cache_key=key,
                output_hash=cached.output_hash,
                reason=["content_hash_match"],
            )
        if token is not None:
            token.checkpoint(f"compute:{point_id}:{node}")
        data, artifacts = self._execute_node(
            scene,
            point_id,
            node,
            results,
            field_key=key if node == "field_coefficients" else None,
        )
        result = self.cache.write_node(point_id, node, key, data, artifacts)
        if token is not None:
            token.checkpoint(f"after:{point_id}:{node}")
        if node in invalidated:
            reasons = ["invalidated"]
        else:
            reasons = ["cache_miss"]
        return result, NodeTrace(
            node=node,
            status="recomputed",
            cache_key=key,
            output_hash=result.output_hash,
            reason=reasons,
        )

    def rebuild(
        self,
        scene: Scene,
        *,
        changes: list[dict[str, Any]] | None = None,
        previous_scene: Scene | None = None,
        mode: str = "incremental",
        token: CancellationToken | None = None,
    ) -> RebuildReport:
        changes = list(changes or [])
        plan = {"points": {}, "factors": []}
        if changes:
            plan = invalidation_plan(previous_scene or scene, changes)
        report = RebuildReport(
            mode=mode,
            scene_id=scene.scene_id,
            factors=list(plan.get("factors", [])),
        )
        for listener in scene.listeners:
            point_id = listener.id
            if token is not None:
                token.checkpoint(f"listener:{point_id}:start")
            point_plan = plan.get("points", {}).get(
                point_id,
                {"invalidated_nodes": [], "reasons": {}, "earliest": None},
            )
            invalidated = list(point_plan.get("invalidated_nodes", []))
            reasons = point_plan.get("reasons", {})
            point_report = PointReport(
                point_id=point_id,
                invalidated_nodes=invalidated,
                reasons={key: list(value) for key, value in reasons.items()},
            )
            results: dict[str, NodeResult] = {}
            with self.locks.acquire(point_id):
                for node in NODE_ORDER:
                    result, trace = self._run_node(
                        scene,
                        point_id,
                        node,
                        results,
                        invalidated,
                        token,
                    )
                    results[node] = result
                    point_report.nodes.append(trace)
                field_result = results["field_coefficients"]
                manifest = dict(field_result.data["field_manifest"])
                if manifest.get("version") != field_result.cache_key:
                    fail(
                        E_CONFIG_INVALID,
                        "field manifest version does not match its content-addressed key",
                        listener_id=point_id,
                    )
                head_entry = {
                    "field_key": field_result.cache_key,
                    "version": field_result.cache_key,
                    "dependency_hash": manifest.get("dependency_hash"),
                    "coefficient_hash": manifest.get("coefficient_hash"),
                    "response_hash": results["multichannel_stitch"].data.get(
                        "response_hash"
                    ),
                }
                self.cache.update_head(point_id, head_entry)
                point_report.field_version = field_result.cache_key
                point_report.coefficient_hash = str(
                    manifest.get("coefficient_hash", "")
                )
                point_report.response_hash = str(
                    results["multichannel_stitch"].data.get("response_hash", "")
                )
                point_report.field_manifest = manifest
                point_report.coefficient_path = str(
                    field_result.artifact_path("coefficients.npy")
                )
                point_report.affected_reflection_events = self._affected_events(
                    point_id,
                    changes,
                    results["early_reflections"].data,
                )
            report.points.append(point_report)
        return report

    def _affected_events(
        self,
        point_id: str,
        changes: list[dict[str, Any]],
        early_data: dict[str, Any],
    ) -> list[str]:
        events = list(early_data.get("events", []))
        affected: list[str] = []
        for change in changes:
            factor = normalize_factor_name(
                change.get("factor", change.get("type", change.get("kind", "")))
            )
            target = change.get(
                "listener_id",
                change.get("listener", change.get("id")),
            )
            if factor == "listener_position" and target != point_id:
                continue
            if factor == "material":
                wall_id = str(change.get("wall_id", change.get("wall", "")))
                selected = [
                    event["event_id"]
                    for event in events
                    if event.get("wall_id") == wall_id
                ]
            else:
                selected = [event["event_id"] for event in events]
            for event_id in selected:
                if event_id not in affected:
                    affected.append(event_id)
        return affected

    def expected_chain(
        self,
        scene: Scene,
        point_id: str,
    ) -> tuple[dict[str, NodeResult], str]:
        results: dict[str, NodeResult] = {}
        for node in NODE_ORDER:
            upstream = self._upstream_for(node, results)
            effective = self._effective(scene, point_id, node)
            key = self._cache_key(node, point_id, upstream, effective)
            result = self.cache.load_node(point_id, node, key)
            if result is None:
                fail(
                    E_FIELD_STALE,
                    "the current dependency chain is not fully materialized",
                    listener_id=point_id,
                    node=node,
                )
            results[node] = result
        return results, results["field_coefficients"].cache_key

    def validate_field(
        self,
        scene: Scene,
        point_id: str,
    ) -> tuple[NodeResult, np.ndarray]:
        heads = self.cache.read_heads()
        head = heads.get(point_id)
        if not isinstance(head, dict):
            fail(
                E_FIELD_STALE,
                "no committed field coefficients exist for this listener",
                listener_id=point_id,
            )
        results, expected_version = self.expected_chain(scene, point_id)
        field_result = results["field_coefficients"]
        manifest = dict(field_result.data.get("field_manifest", {}))
        if (
            head.get("field_key") != expected_version
            or head.get("version") != expected_version
            or manifest.get("version") != expected_version
            or manifest.get("dependency_hash") != expected_version
        ):
            fail(
                E_FIELD_STALE,
                "field coefficient version does not match the dependency manifest",
                listener_id=point_id,
                head_version=head.get("version"),
                expected_version=expected_version,
            )
        dependencies = list(manifest.get("dependencies", []))
        for dependency in dependencies:
            node = dependency.get("node")
            if node not in results:
                fail(
                    E_FIELD_STALE,
                    "field dependency is absent from current cache",
                    dependency=node,
                )
            if results[node].output_hash != dependency.get("output_hash"):
                fail(
                    E_FIELD_STALE,
                    "field dependency content hash has changed",
                    dependency=node,
                )
        coefficients = np.load(field_result.artifact_path("coefficients.npy"))
        if sha256_array(coefficients) != manifest.get("coefficient_hash"):
            fail(
                E_FIELD_STALE,
                "field coefficient file does not match the manifest hash",
                listener_id=point_id,
            )
        return field_result, coefficients

    def render_point(
        self,
        scene: Scene,
        point_id: str,
        *,
        signal_path: str | Path | None = None,
        output_path: str | Path | None = None,
    ) -> dict[str, Any]:
        with self.locks.acquire(point_id):
            field_result, coefficients = self.validate_field(scene, point_id)
            manifest = dict(field_result.data["field_manifest"])
            signal = Path(
                signal_path
                if signal_path is not None
                else scene.resolved_test_signal(self.sample_dir)
            )
            signal_data = read_wav(signal)
            test_digest = sha256_file(signal)
            render_key = sha256_json(
                {
                    "schema": "acoustic-render-key/v1",
                    "implementation": NODE_IMPL_VERSION,
                    "field_version": field_result.cache_key,
                    "test_signal_sha256": test_digest,
                    "render": scene.render,
                }
            )
            cached = self.cache.load_render(point_id, render_key)
            if cached is not None:
                record = cached
            else:
                wav_bytes, metadata = render_field(
                    coefficients,
                    signal_data,
                    scene,
                    field_result.cache_key,
                    list(manifest.get("dependencies", [])),
                )
                metadata["point_id"] = point_id
                metadata["test_signal_path"] = signal.name
                metadata["test_signal_file_sha256"] = test_digest
                record = self.cache.write_render(
                    point_id,
                    render_key,
                    wav_bytes,
                    metadata,
                )
            render_head = {
                "cache_key": render_key,
                "field_version": field_result.cache_key,
                "wav_sha256": record["wav_sha256"],
            }
            self.cache.update_render_head(point_id, render_head)
            if output_path is not None:
                destination = Path(output_path)
                destination.parent.mkdir(parents=True, exist_ok=True)
                source = self.cache.render_path(point_id, render_key)
                temporary = destination.with_name(f".{destination.name}.tmp")
                temporary.write_bytes(source.read_bytes())
                temporary.replace(destination)
            return {
                **record,
                "render_path": str(self.cache.render_path(point_id, render_key)),
            }

    def latest_render(self, point_id: str) -> dict[str, Any] | None:
        field_heads = self.cache.read_heads()
        field_head = field_heads.get(point_id)
        if not isinstance(field_head, dict):
            return None
        heads = self.cache.read_render_heads()
        head = heads.get(point_id)
        if not isinstance(head, dict):
            return None
        record = self.cache.load_render(point_id, str(head.get("cache_key", "")))
        if record is None:
            return None
        if record.get("field_version") != field_head.get("version"):
            return None
        return {
            **record,
            "render_path": str(
                self.cache.render_path(point_id, str(head["cache_key"]))
            ),
        }

    def preview_invalidation(
        self,
        scene: Scene,
        changes: list[dict[str, Any]],
    ) -> dict[str, Any]:
        return invalidation_plan(scene, changes)

    def refresh_scene(self) -> Scene:
        self.scene = Scene.load(self.scene_path)
        self.materials = load_materials(self.sample_dir / self.scene.materials_file)
        return self.scene

    def save_scene(self, scene: Scene) -> None:
        temporary = self.scene_path.with_name(f".{self.scene_path.name}.tmp")
        temporary.write_text(
            canonical_json(scene.to_dict()),
            encoding="utf-8",
        )
        temporary.replace(self.scene_path)
        self.scene = scene

