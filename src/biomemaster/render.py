"""Resumable block renderer and deterministic final mastering."""

from __future__ import annotations

import math
import os
import shutil
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .constants import (
    CHUNK_SAMPLES,
    CROSSFADE_SECONDS,
    TRUE_PEAK_CEILING_DBTP,
)
from .control import StopFlag
from .dsp import (
    crossfade_count,
    db_to_linear,
    mix_biomes_in_order,
    render_biome_chunk,
    soft_limit,
    true_peak_limiter,
)
from .errors import CheckpointCorrupt, RenderFailed
from .loudness import (
    LoudnessState,
    append_ledger_records,
    clone_loudness_state,
    initial_loudness_state,
    integrated_loudness_from_ledger,
    process_loudness_chunk,
    read_ledger,
    verify_ledger_head,
)
from .model import Project
from .report import measure_wav_loudness, sha256_path, write_report
from .storage import (
    CHECKPOINT_FILENAME,
    LEDGER_FILENAME,
    SHARD_CHAIN_INITIAL,
    atomic_write_json,
    cleanup_work_directory,
    fsync_directory,
    read_json_file,
    read_shard_bytes,
    remove_uncommitted_shards,
    shard_bytes_to_audio,
    truncate_ledger,
    update_shard_chain,
    verify_committed_shards,
    write_shard,
)
from .truepeak import true_peak_dbtp_wav

CHECKPOINT_SCHEMA_VERSION = 1
CHECKPOINT_KIND = "biomemaster-render-checkpoint"
MAX_FINALIZATION_ITERATIONS = 10


@dataclass
class ResumeInfo:
    checkpoint: dict[str, Any]
    state: LoudnessState
    resumed: bool
    resume_count: int


def _total_chunks(total_samples: int) -> int:
    return (total_samples + CHUNK_SAMPLES - 1) // CHUNK_SAMPLES


def _chunk_length(total_samples: int, index: int) -> int:
    return min(CHUNK_SAMPLES, total_samples - index * CHUNK_SAMPLES)


def _total_crossfades(project: Project) -> int:
    fade = int(round(CROSSFADE_SECONDS * project.sample_rate))
    return sum(
        crossfade_count(layer.spec.loop_samples, fade, project.total_samples)
        for biome in project.biomes
        for layer in biome.layers
    )


def _checkpoint_payload(
    project: Project,
    *,
    completed_chunks: int,
    previous_state: LoudnessState,
    state: LoudnessState,
    shard_chain_sha256: str,
    latest_shard: dict[str, Any] | None,
    resume_count: int,
    soft_trigger_samples: int,
    soft_trigger_frames: int,
) -> dict[str, Any]:
    return {
        "schema_version": CHECKPOINT_SCHEMA_VERSION,
        "kind": CHECKPOINT_KIND,
        "project_sha256": project.project_sha256,
        "content_sha256": project.content_sha256,
        "sample_rate": project.sample_rate,
        "channels": project.channels,
        "total_samples": project.total_samples,
        "chunk_samples": CHUNK_SAMPLES,
        "total_chunks": _total_chunks(project.total_samples),
        "completed_chunks": completed_chunks,
        "completed_samples": state.processed_samples,
        "crossfade_count": _total_crossfades(project),
        "soft_limiter_trigger_samples": soft_trigger_samples,
        "soft_limiter_trigger_frames": soft_trigger_frames,
        "shard_chain_sha256": shard_chain_sha256,
        "latest_shard": latest_shard,
        "ledger_size_bytes": state.hop_count * 16,
        "ledger_records": state.hop_count,
        "ledger_head_sha256": state.ledger_head_sha256,
        "loudness_prev": previous_state.to_dict(),
        "loudness": state.to_dict(),
        "resume_count": resume_count,
    }


def _discard_work_artifacts(work_dir: Path) -> None:
    if not work_dir.exists():
        work_dir.mkdir(parents=True, exist_ok=True)
        return
    remove_uncommitted_shards(work_dir, 0)
    for name in (
        CHECKPOINT_FILENAME,
        CHECKPOINT_FILENAME + ".tmp",
        LEDGER_FILENAME,
        "final_candidate.wav",
    ):
        (work_dir / name).unlink(missing_ok=True)


def _parse_checkpoint(
    work_dir: Path,
    project: Project,
) -> tuple[dict[str, Any], LoudnessState, LoudnessState] | None:
    path = work_dir / CHECKPOINT_FILENAME
    if not path.is_file():
        return None
    try:
        checkpoint = read_json_file(path)
    except Exception:  # noqa: BLE001 - malformed checkpoints restart by design
        return None
    if not isinstance(checkpoint, dict):
        return None
    required = {
        "schema_version",
        "kind",
        "project_sha256",
        "content_sha256",
        "sample_rate",
        "channels",
        "total_samples",
        "chunk_samples",
        "total_chunks",
        "completed_chunks",
        "completed_samples",
        "shard_chain_sha256",
        "latest_shard",
        "ledger_size_bytes",
        "loudness_prev",
        "loudness",
        "resume_count",
    }
    if not required.issubset(checkpoint):
        return None
    if checkpoint.get("schema_version") != CHECKPOINT_SCHEMA_VERSION:
        return None
    if checkpoint.get("kind") != CHECKPOINT_KIND:
        return None
    if checkpoint.get("project_sha256") != project.project_sha256:
        return None
    if checkpoint.get("content_sha256") != project.content_sha256:
        return None
    try:
        if int(checkpoint["sample_rate"]) != project.sample_rate:
            return None
        if int(checkpoint["channels"]) != project.channels:
            return None
        if int(checkpoint["total_samples"]) != project.total_samples:
            return None
        if int(checkpoint["chunk_samples"]) != CHUNK_SAMPLES:
            return None
        if int(checkpoint["total_chunks"]) != _total_chunks(project.total_samples):
            return None
        if int(checkpoint["resume_count"]) < 0:
            return None
        previous = LoudnessState.from_dict(checkpoint["loudness_prev"])
        current = LoudnessState.from_dict(checkpoint["loudness"])
    except (TypeError, ValueError, CheckpointCorrupt):
        return None
    if (
        previous.sample_rate != project.sample_rate
        or current.sample_rate != project.sample_rate
    ):
        return None
    return checkpoint, previous, current


def _verify_resume(
    work_dir: Path,
    project: Project,
    checkpoint: dict[str, Any],
    previous: LoudnessState,
    current: LoudnessState,
) -> None:
    completed_chunks = int(checkpoint["completed_chunks"])
    total_chunks = _total_chunks(project.total_samples)
    if completed_chunks < 0 or completed_chunks > total_chunks:
        raise CheckpointCorrupt("checkpoint chunk count is outside the project")
    expected_samples = min(project.total_samples, completed_chunks * CHUNK_SAMPLES)
    if current.processed_samples != expected_samples:
        raise CheckpointCorrupt(
            "checkpoint loudness sample count does not match completed chunks"
        )
    if int(checkpoint["completed_samples"]) != expected_samples:
        raise CheckpointCorrupt(
            "checkpoint completed_samples does not match completed chunks"
        )

    remove_uncommitted_shards(work_dir, completed_chunks)
    ledger_path = work_dir / LEDGER_FILENAME
    ledger_size = int(checkpoint["ledger_size_bytes"])
    if ledger_size != current.hop_count * 16:
        raise CheckpointCorrupt(
            "checkpoint ledger byte count is inconsistent with loudness state"
        )
    truncate_ledger(ledger_path, ledger_size)
    records = read_ledger(ledger_path)
    if records.shape[0] != current.hop_count:
        raise CheckpointCorrupt("loudness ledger record count does not match state")
    if verify_ledger_head(records) != current.ledger_head_sha256:
        raise CheckpointCorrupt("loudness ledger digest does not match checkpoint")

    latest_payload = verify_committed_shards(
        work_dir,
        completed_chunks,
        CHUNK_SAMPLES,
        project.total_samples,
        str(checkpoint["shard_chain_sha256"]),
        checkpoint["latest_shard"],
    )

    if completed_chunks == 0:
        if previous.to_dict() != current.to_dict():
            raise CheckpointCorrupt(
                "empty render checkpoint has inconsistent loudness states"
            )
        return

    latest_samples = _chunk_length(project.total_samples, completed_chunks - 1)
    if previous.processed_samples != expected_samples - latest_samples:
        raise CheckpointCorrupt(
            "previous loudness state does not precede the latest shard"
        )
    if latest_payload is None:
        raise CheckpointCorrupt("latest shard bytes are unavailable")
    recomputed, _ = process_loudness_chunk(
        previous, shard_bytes_to_audio(latest_payload)
    )
    if recomputed.to_dict() != current.to_dict():
        raise CheckpointCorrupt(
            "checkpoint loudness intermediate does not match the latest shard"
        )


def _prepare_workspace(
    work_dir: Path,
    project: Project,
    *,
    resume_count_override: int | None = None,
) -> ResumeInfo:
    work_dir.mkdir(parents=True, exist_ok=True)
    parsed = _parse_checkpoint(work_dir, project)
    if parsed is None:
        _discard_work_artifacts(work_dir)
        state = initial_loudness_state(project.sample_rate, project.channels)
        checkpoint = _checkpoint_payload(
            project,
            completed_chunks=0,
            previous_state=state,
            state=state,
            shard_chain_sha256=SHARD_CHAIN_INITIAL,
            latest_shard=None,
            resume_count=0,
            soft_trigger_samples=0,
            soft_trigger_frames=0,
        )
        atomic_write_json(work_dir / CHECKPOINT_FILENAME, checkpoint)
        (work_dir / LEDGER_FILENAME).touch()
        return ResumeInfo(
            checkpoint=checkpoint, state=state, resumed=False, resume_count=0
        )

    checkpoint, previous, current = parsed
    _verify_resume(work_dir, project, checkpoint, previous, current)
    resume_count = int(checkpoint["resume_count"]) + 1
    if resume_count_override is not None:
        resume_count = resume_count_override
    checkpoint["resume_count"] = resume_count
    atomic_write_json(work_dir / CHECKPOINT_FILENAME, checkpoint)
    return ResumeInfo(
        checkpoint=checkpoint,
        state=current,
        resumed=True,
        resume_count=resume_count,
    )


def _render_one_chunk(
    project: Project,
    start: int,
    count: int,
    executor: ThreadPoolExecutor,
) -> np.ndarray:
    fade = int(round(CROSSFADE_SECONDS * project.sample_rate))
    futures = [
        executor.submit(
            render_biome_chunk,
            biome,
            start,
            count,
            project.sample_rate,
            fade,
        )
        for biome in project.biomes
    ]
    try:
        chunks = [future.result() for future in futures]
    except BaseException:
        for future in futures:
            future.cancel()
        raise
    return mix_biomes_in_order(chunks, project.biomes)


def _write_candidate(
    project: Project,
    work_dir: Path,
    completed_chunks: int,
    destination: Path,
    gain_db: float,
    limiter_ceiling_db: float,
    stop: StopFlag,
) -> tuple[bool, int, int]:
    gain = float(db_to_linear(gain_db))
    trigger_samples = 0
    trigger_frames = 0
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.unlink(missing_ok=True)
    with sf.SoundFile(
        str(destination),
        mode="w",
        samplerate=project.sample_rate,
        channels=project.channels,
        format="WAV",
        subtype="PCM_24",
        endian="LITTLE",
    ) as output:
        for index in range(completed_chunks):
            samples = _chunk_length(project.total_samples, index)
            payload = read_shard_bytes(work_dir, index, samples)
            audio = shard_bytes_to_audio(payload)
            normalized = audio * gain
            limited, stats = true_peak_limiter(
                normalized, ceiling_db=limiter_ceiling_db, knee_db=3.0
            )
            output.write(limited)
            trigger_samples += stats.trigger_samples
            trigger_frames += stats.trigger_frames
            if stop.requested:
                destination.unlink(missing_ok=True)
                return False, trigger_samples, trigger_frames
    descriptor = os.open(destination, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    return True, trigger_samples, trigger_frames


def _install_candidate(candidate: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(destination.name + ".tmp")
    with candidate.open("rb") as source, temporary.open("wb") as target:
        shutil.copyfileobj(source, target, length=1024 * 1024)
        target.flush()
        os.fsync(target.fileno())
    os.replace(temporary, destination)
    fsync_directory(destination.parent)


def _finalize_master(
    project: Project,
    work_dir: Path,
    checkpoint: dict[str, Any],
    output_path: Path,
    report_path: Path,
    stop: StopFlag,
) -> dict[str, Any]:
    ledger_path = work_dir / LEDGER_FILENAME
    records = read_ledger(ledger_path)
    current = LoudnessState.from_dict(checkpoint["loudness"])
    if records.shape[0] != current.hop_count:
        raise CheckpointCorrupt("finalization ledger count does not match checkpoint")
    if verify_ledger_head(records) != current.ledger_head_sha256:
        raise CheckpointCorrupt("finalization ledger digest does not match checkpoint")
    measured_lufs = integrated_loudness_from_ledger(records, current)
    if not math.isfinite(measured_lufs):
        raise RenderFailed(
            "rendered program is silent; loudness normalization is undefined"
        )

    gain_db = project.target_loudness_lufs - measured_lufs
    ceiling_db = -1.25
    candidate = work_dir / "final_candidate.wav"
    final_values: dict[str, Any] | None = None
    iterations = 0

    for iterations in range(1, MAX_FINALIZATION_ITERATIONS + 1):
        if stop.requested:
            candidate.unlink(missing_ok=True)
            return {"status": "interrupted"}
        ok, limiter_samples, limiter_frames = _write_candidate(
            project,
            work_dir,
            int(checkpoint["completed_chunks"]),
            candidate,
            gain_db,
            ceiling_db,
            stop,
        )
        if not ok:
            return {"status": "interrupted"}
        final_lufs, frames, sample_rate, channels = measure_wav_loudness(candidate)
        final_tp = true_peak_dbtp_wav(candidate)
        if frames != project.total_samples:
            raise RenderFailed(
                "candidate WAV frame count does not match the project",
                expected=project.total_samples,
                actual=frames,
            )
        final_values = {
            "integrated_loudness_lufs": final_lufs,
            "true_peak_dbtp": final_tp,
            "gain_db": gain_db,
            "ceiling_db": ceiling_db,
            "limiter_samples": limiter_samples,
            "limiter_frames": limiter_frames,
            "iterations": iterations,
            "total_samples": frames,
            "sample_rate": sample_rate,
            "channels": channels,
        }
        loudness_ok = abs(final_lufs - project.target_loudness_lufs) <= 0.3
        peak_ok = final_tp <= TRUE_PEAK_CEILING_DBTP + 1e-9
        if loudness_ok and peak_ok:
            break
        if not peak_ok:
            ceiling_db -= (final_tp - TRUE_PEAK_CEILING_DBTP) + 0.15
            ceiling_db = max(ceiling_db, -12.0)
        if not loudness_ok:
            gain_db += project.target_loudness_lufs - final_lufs
        if stop.requested:
            candidate.unlink(missing_ok=True)
            return {"status": "interrupted"}
    else:
        raise RenderFailed(
            "could not satisfy loudness and true-peak targets",
            final=final_values,
            target_loudness_lufs=project.target_loudness_lufs,
            true_peak_ceiling_dbtp=TRUE_PEAK_CEILING_DBTP,
        )

    assert final_values is not None
    _install_candidate(candidate, output_path)
    candidate.unlink(missing_ok=True)
    wav_sha256 = sha256_path(output_path)
    wav_size = output_path.stat().st_size
    info = sf.info(str(output_path))

    layers_total = sum(len(biome.layers) for biome in project.biomes)
    report = {
        "schema_version": 1,
        "ok": True,
        "project_sha256": project.project_sha256,
        "content_sha256": project.content_sha256,
        "wav_sha256": wav_sha256,
        "wav_size_bytes": wav_size,
        "wav_format": info.format,
        "wav_subtype": info.subtype,
        "sample_rate": project.sample_rate,
        "output_channels": project.channels,
        "duration_seconds": project.total_samples / float(project.sample_rate),
        "total_samples": project.total_samples,
        "layers_total": layers_total,
        "biomes": project.layer_count_by_biome(),
        "target_loudness_lufs": project.target_loudness_lufs,
        "integrated_loudness_lufs": final_values["integrated_loudness_lufs"],
        "loudness_deviation_lu": abs(
            final_values["integrated_loudness_lufs"] - project.target_loudness_lufs
        ),
        "true_peak_dbtp": final_values["true_peak_dbtp"],
        "true_peak_ceiling_dbtp": TRUE_PEAK_CEILING_DBTP,
        "limiter_trigger_samples": int(checkpoint["soft_limiter_trigger_samples"]),
        "limiter_trigger_frames": int(checkpoint["soft_limiter_trigger_frames"]),
        "soft_limiter_trigger_samples": int(checkpoint["soft_limiter_trigger_samples"]),
        "soft_limiter_trigger_frames": int(checkpoint["soft_limiter_trigger_frames"]),
        "true_peak_limiter_trigger_samples": final_values["limiter_samples"],
        "true_peak_limiter_trigger_frames": final_values["limiter_frames"],
        "crossfade_count": int(checkpoint["crossfade_count"]),
        "shard_count": int(checkpoint["completed_chunks"]),
        "checkpoint_resume_count": int(checkpoint["resume_count"]),
        "normalization_gain_db": final_values["gain_db"],
        "final_limiter_ceiling_dbfs": final_values["ceiling_db"],
        "finalization_iterations": final_values["iterations"],
        "loudness_standard": "ITU-R BS.1770-4",
        "true_peak_method": "4x oversampled (windowed-sinc)",
        "loudness_tolerance_lu": 0.3,
    }
    write_report(report_path, report)
    return {"status": "complete", "report": report}


def run_render(
    project: Project,
    *,
    output_path: Path,
    report_path: Path,
    work_dir: Path,
    workers: int,
    stop: StopFlag,
) -> dict[str, Any]:
    info = _prepare_workspace(work_dir, project, resume_count_override=None)
    checkpoint = info.checkpoint
    state = info.state
    completed = int(checkpoint["completed_chunks"])
    total_chunks = _total_chunks(project.total_samples)
    shard_chain = str(checkpoint["shard_chain_sha256"])
    latest_shard = checkpoint["latest_shard"]
    soft_samples = int(checkpoint.get("soft_limiter_trigger_samples", 0))
    soft_frames = int(checkpoint.get("soft_limiter_trigger_frames", 0))
    previous_state = LoudnessState.from_dict(checkpoint["loudness_prev"])
    executor = ThreadPoolExecutor(max_workers=max(1, workers))
    try:
        while completed < total_chunks:
            if stop.requested:
                break
            start = completed * CHUNK_SAMPLES
            samples = _chunk_length(project.total_samples, completed)
            mixed = _render_one_chunk(project, start, samples, executor)
            limited, stats = soft_limit(mixed)
            # The loudness ledger and checkpoint are defined over exactly the
            # values persisted in the float32 shard, not the wider temporary
            # DSP result. This makes resume verification bit-exact.
            limited = np.asarray(limited, dtype="<f4").astype(np.float64)
            soft_samples += stats.trigger_samples
            soft_frames += stats.trigger_frames

            size_bytes, digest = write_shard(work_dir, completed, limited)
            previous_state = clone_loudness_state(state)
            state, records = process_loudness_chunk(state, limited)
            append_ledger_records(work_dir / LEDGER_FILENAME, records)
            shard_chain = update_shard_chain(shard_chain, completed, samples, digest)
            latest_shard = {
                "index": completed,
                "samples": samples,
                "size_bytes": size_bytes,
                "sha256": digest,
            }
            completed += 1
            checkpoint = _checkpoint_payload(
                project,
                completed_chunks=completed,
                previous_state=previous_state,
                state=state,
                shard_chain_sha256=shard_chain,
                latest_shard=latest_shard,
                resume_count=info.resume_count,
                soft_trigger_samples=soft_samples,
                soft_trigger_frames=soft_frames,
            )
            atomic_write_json(work_dir / CHECKPOINT_FILENAME, checkpoint)

            delay_ms = os.environ.get("BIOMEMASTER_CHUNK_DELAY_MS")
            if delay_ms:
                time.sleep(max(0.0, float(delay_ms)) / 1000.0)
    except KeyboardInterrupt:
        stop.request()
    finally:
        executor.shutdown(wait=True, cancel_futures=False)

    if stop.requested:
        return {
            "status": "interrupted",
            "completed_chunks": completed,
            "total_chunks": total_chunks,
            "checkpoint_resume_count": info.resume_count,
        }

    # The final checkpoint is the only source for finalization. This makes the
    # accepted render path identical whether the project ran continuously or
    # resumed after SIGINT/SIGKILL.
    checkpoint = read_json_file(work_dir / CHECKPOINT_FILENAME)
    if int(checkpoint["completed_chunks"]) != total_chunks:
        raise RenderFailed("render checkpoint is unexpectedly incomplete")
    result = _finalize_master(
        project,
        work_dir,
        checkpoint,
        output_path,
        report_path,
        stop,
    )
    if result["status"] == "complete":
        cleanup_work_directory(work_dir)
    return result
