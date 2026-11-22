from __future__ import annotations

import os
import shutil
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .dependency import (
    DependencyPlan,
    plan_dependencies,
    refresh_plan_total,
    with_safe_prefix,
)
from .errors import (
    E_BUSY,
    E_NOT_FOUND,
    E_RECOVERY_INVALID,
    E_SIDECAR_DRIFT,
    TaskError,
    recovery_error,
)
from .models import ResampleRatio, TierParams, WavInfo, sha256_bytes
from .resample import RenderedSlice, render_slice
from .sidecar import (
    SIDECAR_SCHEMA,
    audit_tier,
    build_sidecar,
    estimation_contract,
    make_slice_meta,
    sidecar_bytes,
)
from .storage import (
    SNAPSHOT_SCHEMA,
    PublishedTier,
    TierCatalog,
    VersionStore,
    try_file_lock,
)
from .trace import TraceWriter
from .wavio import load_wav


@dataclass
class GenerationResult:
    tier: TierParams
    run_id: str
    run_dir: Path
    sidecar: dict[str, Any] | None
    tier_version_hash: str | None
    generation: int | None
    generation_hash: str | None
    cancelled: bool
    resumed: bool
    resume_index: int
    reused_slices: int
    recomputed_slices: int
    trace_path: Path
    reused_audio_slices: int = 0
    regenerated_audio_slices: int = 0
    sidecar_recomputed_slices: int = 0


class TranscodeEngine:
    def __init__(
        self,
        store_root: str | os.PathLike[str],
        *,
        trace_callback: Callable[[dict[str, Any]], None] | None = None,
        configure_trace: Callable[[TraceWriter], None] | None = None,
    ):
        self.root = Path(store_root)
        self.store = VersionStore(self.root)
        self.catalog = TierCatalog(self.root)
        self.trace = TraceWriter(
            self.root / "trace.jsonl",
            callback=trace_callback,
        )
        if configure_trace:
            configure_trace(self.trace)
        self._busy: set[str] = set()
        self._busy_lock = threading.Lock()

    def _acquire_busy(self, tier_id: str) -> None:
        with self._busy_lock:
            if tier_id in self._busy:
                raise TaskError(
                    E_BUSY,
                    f"tier is already running: {tier_id}",
                    field="tier_id",
                    details={"tier_id": tier_id},
                )
            self._busy.add(tier_id)

    def _release_busy(self, tier_id: str) -> None:
        with self._busy_lock:
            self._busy.discard(tier_id)

    def _run_id(self, info: WavInfo, tier: TierParams) -> str:
        return (
            f"{tier.tier_id}-v{tier.param_version:06d}-"
            f"{info.source_hash[:12]}-{tier.config_hash()[:8]}"
        )

    def _snapshot(
        self,
        *,
        run_id: str,
        info: WavInfo,
        tier: TierParams,
        total_output_samples: int,
        total_slices: int,
        plan: DependencyPlan,
        previous: PublishedTier | None,
    ) -> dict[str, Any]:
        return {
            "schema": SNAPSHOT_SCHEMA,
            "snapshot_version": 1,
            "run_id": run_id,
            "material_id": info.material_id,
            "source": info.as_dict(),
            "tier": tier.as_dict(),
            "slice_samples": tier.slice_samples,
            "total_output_samples": total_output_samples,
            "total_slices": total_slices,
            "dependency_plan": plan.as_dict(),
            "previous": (
                {
                    "run_id": previous.run_dir.name,
                    "tier_version_hash": previous.tier_version_hash,
                    "generation": previous.generation,
                }
                if previous
                else None
            ),
        }

    @staticmethod
    def _validate_snapshot(
        snapshot: dict[str, Any],
        *,
        run_id: str,
        info: WavInfo,
        tier: TierParams,
        total_output_samples: int,
        total_slices: int,
    ) -> None:
        if snapshot.get("snapshot_version") != 1:
            raise recovery_error(
                "snapshot version is not supported",
                details={"snapshot_version": snapshot.get("snapshot_version")},
            )
        if snapshot.get("run_id") != run_id:
            raise recovery_error(
                "snapshot run id does not match the requested run",
                details={"declared": snapshot.get("run_id"), "expected": run_id},
            )
        if snapshot.get("source", {}).get("source_hash") != info.source_hash:
            raise recovery_error(
                "snapshot source hash does not match the supplied WAV",
                details={
                    "declared": snapshot.get("source", {}).get("source_hash"),
                    "expected": info.source_hash,
                },
            )
        if snapshot.get("source", {}).get("frames") != info.frames:
            raise recovery_error("snapshot source frame count changed")
        if snapshot.get("tier") != tier.as_dict():
            raise recovery_error(
                "snapshot tier parameters are not the requested immutable revision",
                details={"declared": snapshot.get("tier"), "expected": tier.as_dict()},
            )
        if snapshot.get("slice_samples") != tier.slice_samples:
            raise recovery_error("snapshot slice size does not match tier parameters")
        if snapshot.get("total_output_samples") != total_output_samples:
            raise recovery_error(
                "snapshot output length does not match deterministic regeneration",
                details={
                    "declared": snapshot.get("total_output_samples"),
                    "expected": total_output_samples,
                },
            )
        if snapshot.get("total_slices") != total_slices:
            raise recovery_error(
                "snapshot slice count does not match deterministic regeneration",
                details={
                    "declared": snapshot.get("total_slices"),
                    "expected": total_slices,
                },
            )

    def _previous_published(
        self, source_info: WavInfo, tier: TierParams
    ) -> PublishedTier | None:
        try:
            published = self.store.query_tier(tier.tier_id)
        except TaskError as exc:
            if exc.code == E_NOT_FOUND:
                return None
            raise
        if published.sidecar.get("source", {}).get("source_hash") != source_info.source_hash:
            return None
        return published

    def _refine_reuse_plan(
        self,
        plan: DependencyPlan,
        *,
        source: np.ndarray,
        info: WavInfo,
        tier: TierParams,
        total_output_samples: int,
        total_slices: int,
        previous: PublishedTier | None,
    ) -> DependencyPlan:
        if previous is None:
            return with_safe_prefix(plan, safe_prefix=0, total_slices=total_slices)
        old_slices = previous.sidecar.get("slices", [])
        safe_prefix = 0
        for index in range(min(total_slices, len(old_slices))):
            rendered = render_slice(
                source, info, tier, index, total_output_samples
            )
            old = old_slices[index]
            comparisons = (
                rendered.out_start,
                rendered.out_end,
                rendered.src_start,
                rendered.src_end,
                rendered.phase_num,
                rendered.phase_den,
                rendered.phase_remainder_num,
                rendered.end_phase_num,
                rendered.peak_abs,
                rendered.silent,
                rendered.content_hash,
            )
            expected = (
                old.get("out_start"),
                old.get("out_end"),
                old.get("src_start"),
                old.get("src_end"),
                old.get("phase_num"),
                old.get("phase_den"),
                old.get("phase_remainder_num"),
                old.get("end_phase_num"),
                old.get("peak_abs"),
                old.get("silent"),
                old.get("content_hash"),
            )
            if comparisons != expected:
                break
            safe_prefix += 1
        return with_safe_prefix(
            plan,
            safe_prefix=safe_prefix,
            total_slices=total_slices,
        )

    def _copy_or_generate(
        self,
        *,
        run_dir: Path,
        index: int,
        rendered: RenderedSlice,
        previous: PublishedTier | None,
        previous_chain: str,
        plan: DependencyPlan,
    ) -> tuple[dict[str, Any], bool, bool]:
        if index < plan.full_reuse_prefix and previous is not None:
            pcm = self.store.read_slice_audio(previous.run_dir, index)
            meta = self.store.read_slice_meta(previous.run_dir, index)
            if meta.get("content_hash") != sha256_bytes(pcm):
                raise recovery_error(
                    "published audio and sidecar differ while attempting reuse",
                    index,
                )
            if meta.get("content_hash") != rendered.content_hash:
                raise recovery_error(
                    "reuse plan was invalid: rematerialized slice differs",
                    index,
                )
            self.store.commit_slice(run_dir, index, pcm, meta)
            return meta, True, True
        meta = make_slice_meta(rendered, previous_chain)
        audio_source: Path | None = None
        audio_reused = False
        if previous is not None:
            old_slices = previous.sidecar.get("slices", [])
            if index < len(old_slices):
                old_meta = old_slices[index]
                if old_meta.get("content_hash") == rendered.content_hash:
                    candidate = self.store.slice_dir(previous.run_dir, index) / "audio.pcm"
                    if candidate.is_file():
                        audio_source = candidate
                        audio_reused = True
        self.store.commit_slice(
            run_dir,
            index,
            rendered.pcm_bytes,
            meta,
            audio_source=audio_source,
        )
        return meta, False, audio_reused

    def _validate_committed_prefix(
        self,
        *,
        run_dir: Path,
        source: np.ndarray,
        info: WavInfo,
        tier: TierParams,
        total_output_samples: int,
        total_slices: int,
    ) -> int:
        indices = self.store.list_committed_indices(run_dir)
        expected_index = 0
        previous_chain = ""
        previous_meta: dict[str, Any] | None = None
        for index in indices:
            if index != expected_index:
                raise recovery_error(
                    "committed slice sequence has a gap or out-of-order slice",
                    expected_index,
                    committed_indices=indices,
                )
            if index >= total_slices:
                raise recovery_error("committed slice lies beyond snapshot total", index)
            pcm = self.store.read_slice_audio(run_dir, index)
            meta = self.store.read_slice_meta(run_dir, index)
            expected = render_slice(source, info, tier, index, total_output_samples)
            expected_meta = make_slice_meta(expected, previous_chain)
            problems: list[str] = []
            if sha256_bytes(pcm) != meta.get("content_hash"):
                problems.append("content_hash")
            if pcm != expected.pcm_bytes:
                problems.append("pcm_samples")
            for field, value in expected_meta.items():
                if meta.get(field) != value:
                    problems.append(field)
            if problems:
                raise recovery_error(
                    "committed slice fails hash-chain or sample-level validation",
                    index,
                    problems=sorted(set(problems)),
                )
            previous_chain = meta["chain_hash"]
            previous_meta = meta
            expected_index += 1

        if expected_index < total_slices and previous_meta is not None:
            rendered = render_slice(
                source, info, tier, expected_index, total_output_samples
            )
            ratio = ResampleRatio.build(info.sample_rate, tier.target_rate)
            expected_handoff = (
                previous_meta["end_phase_num"]
                + (tier.filter_length // 2) * ratio.step_num
            ) % ratio.step_den
            if (
                rendered.phase_num != previous_meta["end_phase_num"]
                or rendered.phase_remainder_num != expected_handoff
            ):
                raise recovery_error(
                    "next uncommitted slice does not hand off from the committed phase remainder",
                    expected_index,
                    previous_end_phase=previous_meta["end_phase_num"],
                    expected_phase=rendered.phase_num,
                    expected_phase_remainder=expected_handoff,
                    actual_phase_remainder=rendered.phase_remainder_num,
                )
        return expected_index

    def _load_complete_run(
        self,
        *,
        run_dir: Path,
        source: np.ndarray,
        info: WavInfo,
        tier: TierParams,
        total_output_samples: int,
    ) -> tuple[dict[str, Any], dict[str, Any], list[bytes]]:
        sidecar = self.store.load_run_sidecar(run_dir)
        version = self.store.load_run_version(run_dir)
        if sidecar.get("schema") != SIDECAR_SCHEMA:
            raise recovery_error("complete run sidecar schema is invalid")
        if sidecar.get("tier_params") != tier.as_dict():
            raise recovery_error("complete run tier parameters changed")
        if sidecar.get("source", {}).get("source_hash") != info.source_hash:
            raise recovery_error("complete run source hash changed")
        if sidecar.get("total_output_samples") != total_output_samples:
            raise recovery_error("complete run output length changed")
        audio: list[bytes] = []
        for index in range(int(sidecar.get("total_slices", 0))):
            audio.append(self.store.read_slice_audio(run_dir, index))
        try:
            audit_tier(
                source,
                info,
                tier,
                sidecar,
                audio,
                expected_version_hash=version.get("tier_version_hash"),
            )
        except TaskError as exc:
            if exc.code == E_SIDECAR_DRIFT:
                raise recovery_error(
                    "complete run fails sample-level sidecar audit",
                    details=exc.details,
                ) from exc
            raise
        return sidecar, version, audio

    def _publish_if_needed(
        self,
        *,
        tier: TierParams,
        run_id: str,
        run_dir: Path,
        sidecar: dict[str, Any],
        info: WavInfo,
    ) -> dict[str, Any]:
        version_hash = sidecar["tier_version_hash"]
        payload = sidecar_bytes(sidecar)
        manifest = self.store.publish_tier(
            tier=tier,
            run_id=run_id,
            run_dir=run_dir,
            tier_version_hash=version_hash,
            sidecar_path_digest=sha256_bytes(payload),
            material_id=info.material_id,
            source_hash=info.source_hash,
        )
        return manifest

    def generate_tier(
        self,
        wav_path: str | os.PathLike[str],
        tier: TierParams,
        *,
        cancel_event: threading.Event | None = None,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> GenerationResult:
        self._acquire_busy(tier.tier_id)
        try:
            return self._generate_tier_locked(
                wav_path,
                tier,
                cancel_event=cancel_event,
                progress=progress,
            )
        finally:
            self._release_busy(tier.tier_id)

    def _generate_tier_locked(
        self,
        wav_path: str | os.PathLike[str],
        tier: TierParams,
        *,
        cancel_event: threading.Event | None,
        progress: Callable[[dict[str, Any]], None] | None,
    ) -> GenerationResult:
        info, source = load_wav(wav_path)
        ratio = ResampleRatio.build(info.sample_rate, tier.target_rate)
        total_output_samples = ratio.output_length(info.frames)
        total_slices = (total_output_samples + tier.slice_samples - 1) // tier.slice_samples
        run_id = self._run_id(info, tier)
        run_dir = self.store.run_dir(tier.tier_id, run_id)
        run_lock_path = self.store.run_lock_path(tier.tier_id)

        with try_file_lock(run_lock_path, tier_id=tier.tier_id):
            previous = self._previous_published(info, tier)
            resumed = (run_dir / "snapshot.json").is_file()
            resume_index = 0
            if resumed:
                snapshot = self.store.load_snapshot(run_dir)
                self._validate_snapshot(
                    snapshot,
                    run_id=run_id,
                    info=info,
                    tier=tier,
                    total_output_samples=total_output_samples,
                    total_slices=total_slices,
                )
                plan = refresh_plan_total(
                    DependencyPlan.from_dict(snapshot["dependency_plan"]),
                    total_slices,
                )
                resume_index = self._validate_committed_prefix(
                    run_dir=run_dir,
                    source=source,
                    info=info,
                    tier=tier,
                    total_output_samples=total_output_samples,
                    total_slices=total_slices,
                )
                self.trace.emit(
                    "recovery_scan",
                    material_id=info.material_id,
                    tier_id=tier.tier_id,
                    param_version=tier.param_version,
                    run_id=run_id,
                    committed_prefix_slices=resume_index,
                    total_slices=total_slices,
                    next_slice=resume_index,
                )
                if progress:
                    progress(
                        {
                            "event": "recovery",
                            "tier_id": tier.tier_id,
                            "resume_index": resume_index,
                            "total_slices": total_slices,
                        }
                    )
            else:
                if run_dir.exists():
                    children = list(run_dir.iterdir())
                    non_temporary = [child for child in children if not child.name.startswith(".tmp-")]
                    if non_temporary:
                        raise recovery_error(
                            "run directory exists without a snapshot",
                            details={"run_id": run_id},
                        )
                    shutil.rmtree(run_dir, ignore_errors=True)
                plan = plan_dependencies(
                    TierParams(**previous.sidecar["tier_params"]) if previous else None,
                    tier,
                    previous.sidecar if previous else None,
                )
                plan = refresh_plan_total(plan, total_slices)
                plan = self._refine_reuse_plan(
                    plan,
                    source=source,
                    info=info,
                    tier=tier,
                    total_output_samples=total_output_samples,
                    total_slices=total_slices,
                    previous=previous,
                )
                snapshot = self._snapshot(
                    run_id=run_id,
                    info=info,
                    tier=tier,
                    total_output_samples=total_output_samples,
                    total_slices=total_slices,
                    plan=plan,
                    previous=previous,
                )
                self.store.write_snapshot(run_dir, snapshot)
                contract = estimation_contract(info, tier, ratio)
                self.trace.emit(
                    "estimation_contract",
                    material_id=info.material_id,
                    tier_id=tier.tier_id,
                    param_version=tier.param_version,
                    run_id=run_id,
                    source_data_hash=info.source_hash,
                    observation_window=contract["observation_window"],
                    sync_markers=contract["sync_markers"],
                    truth_source=contract["truth_source"],
                    phase_definition=contract["phase_definition"],
                )
                self.trace.emit(
                    "dependency_plan",
                    material_id=info.material_id,
                    tier_id=tier.tier_id,
                    param_version=tier.param_version,
                    run_id=run_id,
                    changed_fields=list(plan.changed_fields),
                    direct_dirty_slices=len(plan.direct_dirty),
                    reused_slices=plan.reused_slices,
                    recomputed_slices=plan.recomputed_slices,
                    total_slices=total_slices,
                    phase_changed=plan.phase_changed,
                    propagation_from=plan.propagation_from,
                )

            if self.store.is_complete(run_dir):
                sidecar, version, _audio = self._load_complete_run(
                    run_dir=run_dir,
                    source=source,
                    info=info,
                    tier=tier,
                    total_output_samples=total_output_samples,
                )
                manifest = self._publish_if_needed(
                    tier=tier,
                    run_id=run_id,
                    run_dir=run_dir,
                    sidecar=sidecar,
                    info=info,
                )
                self.trace.emit(
                    "tier_complete_recovered",
                    material_id=info.material_id,
                    tier_id=tier.tier_id,
                    param_version=tier.param_version,
                    run_id=run_id,
                    tier_version_hash=sidecar["tier_version_hash"],
                    generation=manifest["generation"],
                )
                return GenerationResult(
                    tier=tier,
                    run_id=run_id,
                    run_dir=run_dir,
                    sidecar=sidecar,
                    tier_version_hash=sidecar["tier_version_hash"],
                    generation=int(manifest["generation"]),
                    generation_hash=manifest["generation_hash"],
                    cancelled=False,
                    resumed=resumed,
                    resume_index=resume_index,
                    reused_slices=plan.reused_slices,
                    recomputed_slices=plan.recomputed_slices,
                    trace_path=self.trace.path,
                )

            reused_this_run = 0
            generated_this_run = 0
            reused_audio = 0
            regenerated_audio = 0
            recomputed_sidecars = 0
            previous_chain = ""
            if resume_index > 0:
                previous_chain = self.store.read_slice_meta(
                    run_dir, resume_index - 1
                )["chain_hash"]

            for index in range(resume_index, total_slices):
                if cancel_event is not None and cancel_event.is_set():
                    self.trace.emit(
                        "tier_cancelled",
                        material_id=info.material_id,
                        tier_id=tier.tier_id,
                        param_version=tier.param_version,
                        run_id=run_id,
                        committed_slices=index,
                        total_slices=total_slices,
                    )
                    if progress:
                        progress(
                            {
                                "event": "cancelled",
                                "tier_id": tier.tier_id,
                                "committed_slices": index,
                                "total_slices": total_slices,
                            }
                        )
                    return GenerationResult(
                        tier=tier,
                        run_id=run_id,
                        run_dir=run_dir,
                        sidecar=None,
                        tier_version_hash=None,
                        generation=None,
                        generation_hash=None,
                        cancelled=True,
                        resumed=resumed,
                        resume_index=resume_index,
                        reused_slices=reused_this_run,
                        recomputed_slices=generated_this_run,
                        trace_path=self.trace.path,
                        reused_audio_slices=reused_audio,
                        regenerated_audio_slices=regenerated_audio,
                        sidecar_recomputed_slices=recomputed_sidecars,
                    )
                rendered = render_slice(
                    source, info, tier, index, total_output_samples
                )
                if cancel_event is not None and cancel_event.is_set():
                    continue
                meta, reused, audio_reused = self._copy_or_generate(
                    run_dir=run_dir,
                    index=index,
                    rendered=rendered,
                    previous=previous,
                    previous_chain=previous_chain,
                    plan=plan,
                )
                previous_chain = meta["chain_hash"]
                if reused:
                    reused_this_run += 1
                    reused_audio += 1
                    status = "reused"
                else:
                    generated_this_run += 1
                    recomputed_sidecars += 1
                    if audio_reused:
                        reused_audio += 1
                    else:
                        regenerated_audio += 1
                    status = "recomputed"
                chain = plan.propagation_chain(index)
                self.trace.emit(
                    "slice_reused" if reused else "slice_recomputed",
                    material_id=info.material_id,
                    tier_id=tier.tier_id,
                    param_version=tier.param_version,
                    run_id=run_id,
                    slice_index=index,
                    phase_remainder_num=meta["phase_remainder_num"],
                    content_hash=meta["content_hash"],
                    reason=(
                        "unchanged_prefix"
                        if reused
                        else plan.reasons.get(index, "forced")
                    ),
                    propagation_chain=chain,
                    reused_slices=plan.reused_slices,
                    recomputed_slices=plan.recomputed_slices,
                    recovered_prefix_slices=resume_index,
                    audio_reused=audio_reused or reused,
                    sidecar_recomputed=not reused,
                )
                if progress:
                    progress(
                        {
                            "event": "slice",
                            "status": status,
                            "tier_id": tier.tier_id,
                            "slice_index": index,
                            "total_slices": total_slices,
                            "phase_remainder_num": meta["phase_remainder_num"],
                            "content_hash": meta["content_hash"],
                            "audio_reused": audio_reused or reused,
                            "sidecar_recomputed": not reused,
                        }
                    )

            metas = [
                self.store.read_slice_meta(run_dir, index)
                for index in range(total_slices)
            ]
            sidecar = build_sidecar(
                info,
                tier,
                metas,
                total_output_samples,
            )
            audio = [
                self.store.read_slice_audio(run_dir, index)
                for index in range(total_slices)
            ]
            try:
                audit_tier(
                    source,
                    info,
                    tier,
                    sidecar,
                    audio,
                    expected_version_hash=sidecar["tier_version_hash"],
                )
            except TaskError as exc:
                if resumed and exc.code == E_SIDECAR_DRIFT:
                    raise recovery_error(
                        "resumed run failed sample-level sidecar reconciliation",
                        details=exc.details,
                    ) from exc
                raise

            descriptor = {
                "run_id": run_id,
                "tier_id": tier.tier_id,
                "tier_version_hash": sidecar["tier_version_hash"],
                "param_version": tier.param_version,
                "sidecar_digest": sha256_bytes(sidecar_bytes(sidecar)),
                "source_hash": info.source_hash,
                "material_id": info.material_id,
            }
            self.store.finalize_run(run_dir, sidecar, descriptor)
            manifest = self._publish_if_needed(
                tier=tier,
                run_id=run_id,
                run_dir=run_dir,
                sidecar=sidecar,
                info=info,
            )
            self.trace.emit(
                "tier_complete",
                material_id=info.material_id,
                tier_id=tier.tier_id,
                param_version=tier.param_version,
                run_id=run_id,
                tier_version_hash=sidecar["tier_version_hash"],
                generation=manifest["generation"],
                reused_slices=plan.reused_slices,
                recomputed_slices=plan.recomputed_slices,
                reused_this_run=reused_this_run,
                generated_this_run=generated_this_run,
                reused_audio_slices=reused_audio,
                regenerated_audio_slices=regenerated_audio,
                sidecar_recomputed_slices=recomputed_sidecars,
                recovered_prefix_slices=resume_index,
                total_slices=total_slices,
            )
            if progress:
                progress(
                    {
                        "event": "complete",
                        "tier_id": tier.tier_id,
                        "generation": manifest["generation"],
                        "generation_hash": manifest["generation_hash"],
                        "tier_version_hash": sidecar["tier_version_hash"],
                    }
                )
            return GenerationResult(
                tier=tier,
                run_id=run_id,
                run_dir=run_dir,
                sidecar=sidecar,
                tier_version_hash=sidecar["tier_version_hash"],
                generation=int(manifest["generation"]),
                generation_hash=manifest["generation_hash"],
                cancelled=False,
                resumed=resumed,
                resume_index=resume_index,
                reused_slices=plan.reused_slices,
                recomputed_slices=plan.recomputed_slices,
                trace_path=self.trace.path,
                reused_audio_slices=reused_audio,
                regenerated_audio_slices=regenerated_audio,
                sidecar_recomputed_slices=recomputed_sidecars,
            )

    def audit_published_tier(
        self,
        wav_path: str | os.PathLike[str],
        tier_id: str,
        *,
        expected_manifest_hash: str | None = None,
    ) -> dict[str, Any]:
        info, source = load_wav(wav_path)
        published = self.store.query_tier(
            tier_id, expected_manifest_hash=expected_manifest_hash
        )
        sidecar = published.sidecar
        if sidecar.get("source", {}).get("source_hash") != info.source_hash:
            raise TaskError(
                E_SIDECAR_DRIFT,
                "published tier belongs to a different source WAV",
                details={
                    "first_drift_slice": 0,
                    "stale_side": "sidecar",
                    "declared_source_hash": sidecar.get("source", {}).get("source_hash"),
                    "actual_source_hash": info.source_hash,
                },
            )
        tier = TierParams(**sidecar["tier_params"])
        report = audit_tier(
            source,
            info,
            tier,
            sidecar,
            published.audio_slices,
            expected_version_hash=published.tier_version_hash,
        )
        report["generation"] = published.generation
        report["generation_hash"] = published.generation_hash
        report["manifest_hash"] = published.manifest_hash
        return report

    def audit_committed_run(
        self,
        wav_path: str | os.PathLike[str],
        run_dir: str | os.PathLike[str],
    ) -> dict[str, Any]:
        info, source = load_wav(wav_path)
        target = Path(run_dir)
        sidecar = self.store.load_run_sidecar(target)
        tier = TierParams(**sidecar["tier_params"])
        total_slices = int(sidecar.get("total_slices", 0))
        audio: list[bytes] = []
        for index in range(total_slices):
            audio.append(self.store.read_slice_audio(target, index))
        try:
            report = audit_tier(
                source,
                info,
                tier,
                sidecar,
                audio,
                expected_version_hash=sidecar.get("tier_version_hash"),
            )
        except TaskError as exc:
            if exc.code == E_SIDECAR_DRIFT:
                exc.details.setdefault("run_dir", str(target))
            raise
        report["run_id"] = target.name
        report["run_dir"] = str(target)
        return report

