"""End-to-end deterministic audiobook mastering and reporting."""

from __future__ import annotations

import math
import os
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .config import ValidatedBook, load_and_validate
from .dsp import (
    DB_FLOOR,
    LimitResult,
    apply_boundary_level_match,
    apply_noise_safe_gain,
    boundary_levels,
    integrated_loudness,
    limit_true_peak,
    noise_floor_dbfs,
    sample_peak_dbfs,
    true_peak_dbtp,
)
from .state import ResumeState, SnapshotManager, StateExpectation
from .util import atomic_write_json, display_path, sha256_file
from .wavio import (
    decode_pcm_bytes,
    quantize_pcm,
    read_wav,
    write_wav_from_pcm,
)

LOUDNESS_TOLERANCE_LU = 0.10
MAX_SAFETY_CLAMP_RUN = 8


@dataclass(slots=True)
class Candidate:
    gains_db: np.ndarray
    global_correction_db: float
    processed: list[np.ndarray]
    quantized: list[np.ndarray]
    limiter_results: list[LimitResult]
    static_gain_db: np.ndarray
    activity_gain_db: np.ndarray
    boundary_trims_db: dict[int, float]
    boundary_measures: list[dict[str, float]]
    chapter_loudness_lufs: np.ndarray
    full_loudness_lufs: float
    full_true_peak_dbtp: float


def inspect_packages(
    rules_path: str | Path,
    manifest_path: str | Path,
) -> dict[str, Any]:
    book = load_and_validate(rules_path, manifest_path)
    chapters: list[dict[str, Any]] = []
    total_frames = 0
    for chapter in book.chapters:
        total_frames += chapter.plan.rule.expected_frames
        packages = []
        for package in chapter.packages:
            audio = package.audio
            packages.append(
                {
                    "file": package.plan.report_file,
                    "batch": package.plan.batch,
                    "sample_rate": audio.info.sample_rate,
                    "channels": audio.info.channels,
                    "bit_depth": audio.info.bit_depth,
                    "frames": audio.info.frames,
                    "effective_samples": audio.info.frames,
                    "duration_seconds": audio.info.frames
                    / audio.info.sample_rate,
                    "peak_dbfs": sample_peak_dbfs(audio.samples),
                    "true_peak_dbtp": true_peak_dbtp(audio.samples),
                    "sha256": package.sha256,
                }
            )
        chapters.append(
            {
                "id": chapter.plan.rule.id,
                "expected_duration_seconds": chapter.plan.rule.expected_duration_seconds,
                "expected_frames": chapter.plan.rule.expected_frames,
                "source_sample_rate": chapter.source_sample_rate,
                "resolved_channels": book.master_channels,
                "input_loudness_lufs": integrated_loudness(
                    chapter.master_samples,
                    book.rules.sample_rate,
                    relative_gate_lu=book.rules.relative_gate_lu,
                ),
                "packages": packages,
            }
        )
    return {
        "ok": True,
        "status": "inspected",
        "rules": {
            "sample_rate": book.rules.sample_rate,
            "bit_depth": book.rules.bit_depth,
            "target_loudness_lufs": book.rules.target_loudness_lufs,
            "relative_gate_lu": book.rules.relative_gate_lu,
            "true_peak_dbtp": book.rules.true_peak_dbtp,
            "max_noise_floor_rise_db": book.rules.max_noise_floor_rise_db,
            "boundary_tolerance_db": book.rules.boundary_tolerance_db,
            "block_frames": book.rules.block_frames,
        },
        "chapters": chapters,
        "totals": {
            "chapters": len(chapters),
            "packages": sum(len(chapter["packages"]) for chapter in chapters),
            "frames": total_frames,
            "duration_seconds": total_frames / book.rules.sample_rate,
            "channels": book.master_channels,
            "rule_hash": book.rules.content_hash,
            "input_digest": book.input_digest,
        },
    }


def render_book(
    rules_path: str | Path,
    manifest_path: str | Path,
    master_path: str | Path,
    report_path: str | Path,
    state_dir: str | Path,
    *,
    stop_requested: Callable[[], bool] | None = None,
    interrupt_after_blocks: int | None = None,
) -> dict[str, Any]:
    """Render a book.  Returns a status payload and writes the report."""

    rules_path = Path(rules_path)
    manifest_path = Path(manifest_path)
    master_path = Path(master_path)
    report_path = Path(report_path)
    state_dir = Path(state_dir)
    if interrupt_after_blocks is not None and interrupt_after_blocks < 1:
        raise ValueError("interrupt_after_blocks must be positive")

    # Validation happens before any mastering artifact is written.
    book = load_and_validate(rules_path, manifest_path)
    rules = book.rules
    total_frames = sum(
        chapter.plan.rule.expected_frames for chapter in book.chapters
    )
    total_blocks = int(math.ceil(total_frames / rules.block_frames))
    codec = {
        "sample_rate": rules.sample_rate,
        "channels": book.master_channels,
        "bit_depth": rules.bit_depth,
    }
    expectation = StateExpectation(
        state_dir=state_dir,
        rule_hash=rules.content_hash,
        input_digest=book.input_digest,
        codec=codec,
        chunk_frames=rules.block_frames,
        total_frames=total_frames,
        total_blocks=total_blocks,
        input_summary=list(book.input_summary),
    )
    manager = SnapshotManager(expectation)
    resume = manager.open()
    manager.initialize_hasher(resume=resume.completed_frames > 0)

    before_chapter = [
        integrated_loudness(
            chapter.master_samples,
            rules.sample_rate,
            relative_gate_lu=rules.relative_gate_lu,
        )
        for chapter in book.chapters
    ]
    before_full = integrated_loudness(
        np.concatenate(
            [chapter.master_samples for chapter in book.chapters], axis=0
        ),
        rules.sample_rate,
        relative_gate_lu=rules.relative_gate_lu,
    )
    base_gains = np.array(
        [
            rules.target_loudness_lufs - loudness
            if loudness > DB_FLOOR + 1.0
            else 0.0
            for loudness in before_chapter
        ],
        dtype=np.float64,
    )
    candidate = _decide_processing(book, base_gains)
    pcm_path = manager.pcm_path
    pcm_path.parent.mkdir(parents=True, exist_ok=True)
    mode = "ab" if resume.completed_frames else "wb"
    stopped = False
    completed_blocks = resume.completed_blocks
    completed_frames = resume.completed_frames
    chapter_blocks_done, chapter_frames_done = _progress_counts(
        book, completed_frames, rules.block_frames
    )
    with pcm_path.open(mode) as pcm_handle:
        start_block = resume.completed_blocks
        for block_index in range(start_block, total_blocks):
            start = block_index * rules.block_frames
            end = min(total_frames, start + rules.block_frames)
            block = _slice_master(candidate.processed, start, end)
            payload = quantize_pcm(block, rules.bit_depth)
            pcm_handle.write(payload)
            pcm_handle.flush()
            os.fsync(pcm_handle.fileno())
            manager.update_pcm_hash(payload)
            completed_blocks = block_index + 1
            completed_frames = end
            _update_progress_counts(
                book,
                start,
                end,
                chapter_blocks_done,
                chapter_frames_done,
            )
            manager.commit(
                completed_blocks=completed_blocks,
                completed_frames=completed_frames,
                chapter_blocks=chapter_blocks_done,
                chapter_frames=chapter_frames_done,
                status=(
                    "complete" if completed_blocks == total_blocks else "rendering"
                ),
            )
            should_stop = (
                stop_requested is not None and stop_requested()
            ) or (
                interrupt_after_blocks is not None
                and completed_blocks >= interrupt_after_blocks
            )
            if should_stop and completed_blocks < total_blocks:
                stopped = True
                break

    if stopped:
        recovery = _recovery_payload(
            resume,
            manager,
            completed_blocks=completed_blocks,
            completed_frames=completed_frames,
            total_blocks=total_blocks,
            total_frames=total_frames,
            status="interrupted",
            business_code="E_INTERRUPTED",
        )
        report = _interrupted_report(
            book=book,
            master_path=master_path,
            report_path=report_path,
            candidate=candidate,
            before_chapter=before_chapter,
            before_full=before_full,
            progress=recovery,
            chapter_blocks=chapter_blocks_done,
            chapter_frames=chapter_frames_done,
        )
        atomic_write_json(report_path, report)
        return {
            "ok": True,
            "status": "interrupted",
            "business_code": "E_INTERRUPTED",
            "master": None,
            "report": display_path(report_path, report_path.parent),
            "recovery": report["recovery"],
            "progress": report["progress"],
        }

    pcm = pcm_path.read_bytes()
    expected_bytes = total_frames * book.master_channels * (rules.bit_depth // 8)
    if len(pcm) != expected_bytes:
        raise RuntimeError(
            f"state PCM length {len(pcm)} does not equal expected {expected_bytes}"
        )
    write_wav_from_pcm(
        master_path,
        pcm,
        sample_rate=rules.sample_rate,
        channels=book.master_channels,
        bit_depth=rules.bit_depth,
    )
    report = _complete_report(
        book=book,
        master_path=master_path,
        report_path=report_path,
        candidate=candidate,
        before_chapter=before_chapter,
        before_full=before_full,
        recovery=_recovery_payload(
            resume,
            manager,
            completed_blocks=total_blocks,
            completed_frames=total_frames,
            total_blocks=total_blocks,
            total_frames=total_frames,
            status="complete",
            business_code=resume.business_code,
        ),
    )
    atomic_write_json(report_path, report)
    return {
        "ok": True,
        "status": "complete",
        "business_code": resume.business_code,
        "master": display_path(master_path, report_path.parent),
        "report": display_path(report_path, report_path.parent),
        "recovery": report["recovery"],
        "validation": report["validation"],
    }


def _decide_processing(
    book: ValidatedBook,
    base_gains: np.ndarray,
) -> Candidate:
    rules = book.rules
    gains = np.array(base_gains, dtype=np.float64, copy=True)
    best: Candidate | None = None
    best_score = float("inf")
    for _ in range(12):
        candidate = _render_candidate(
            book,
            gains,
            float(np.mean(gains - base_gains)),
        )
        book_error = rules.target_loudness_lufs - candidate.full_loudness_lufs
        chapter_mean = float(np.mean(candidate.chapter_loudness_lufs))
        alignment_errors = chapter_mean - candidate.chapter_loudness_lufs
        score = max(
            float(np.max(np.abs(alignment_errors))),
            abs(book_error),
        )
        if score < best_score:
            best = candidate
            best_score = score
        if score <= 0.03:
            break
        proposed = np.clip(
            gains + 0.85 * (alignment_errors + book_error),
            -60.0,
            60.0,
        )
        if np.max(np.abs(proposed - gains)) < 1e-7:
            break
        gains = proposed
    assert best is not None
    return best


def _render_candidate(
    book: ValidatedBook,
    gains_db: np.ndarray,
    global_correction: float,
) -> Candidate:
    rules = book.rules
    processed: list[np.ndarray] = []
    limit_results: list[LimitResult] = []
    static_gains = []
    activity_gains = []
    for chapter, gain_db in zip(book.chapters, gains_db):
        raised, static_db, extra_db = apply_noise_safe_gain(
            chapter.master_samples,
            rules.sample_rate,
            float(gain_db),
            rules.max_noise_floor_rise_db,
        )
        limited = limit_true_peak(
            raised, rules.sample_rate, rules.true_peak_dbtp
        )
        processed.append(limited.samples)
        limit_results.append(limited)
        static_gains.append(static_db)
        activity_gains.append(extra_db)
    processed, trims, measures = apply_boundary_level_match(
        processed, rules.sample_rate, rules.boundary_tolerance_db
    )
    quantized: list[np.ndarray] = []
    for chapter in processed:
        payload = quantize_pcm(chapter, rules.bit_depth)
        quantized.append(
            decode_pcm_bytes(payload, rules.bit_depth, book.master_channels)
        )
    master = np.concatenate(quantized, axis=0)
    chapter_loudness = []
    for chapter in quantized:
        chapter_loudness.append(
            integrated_loudness(
                chapter,
                rules.sample_rate,
                relative_gate_lu=rules.relative_gate_lu,
            )
        )
    return Candidate(
        gains_db=np.array(gains_db, copy=True),
        global_correction_db=global_correction,
        processed=processed,
        quantized=quantized,
        limiter_results=limit_results,
        static_gain_db=np.asarray(static_gains, dtype=np.float64),
        activity_gain_db=np.asarray(activity_gains, dtype=np.float64),
        boundary_trims_db=trims,
        boundary_measures=measures,
        chapter_loudness_lufs=np.asarray(chapter_loudness, dtype=np.float64),
        full_loudness_lufs=integrated_loudness(
            master,
            rules.sample_rate,
            relative_gate_lu=rules.relative_gate_lu,
        ),
        full_true_peak_dbtp=true_peak_dbtp(master),
    )


def _slice_master(chapters: list[np.ndarray], start: int, end: int) -> np.ndarray:
    pieces: list[np.ndarray] = []
    cursor = 0
    for chapter in chapters:
        chapter_end = cursor + chapter.shape[0]
        if end <= cursor:
            break
        if start < chapter_end and end > cursor:
            local_start = max(0, start - cursor)
            local_end = min(chapter.shape[0], end - cursor)
            pieces.append(chapter[local_start:local_end])
        cursor = chapter_end
    if not pieces:
        return np.zeros((0, chapters[0].shape[1]), dtype=np.float64)
    return np.concatenate(pieces, axis=0)


def _progress_counts(
    book: ValidatedBook, frames: int, chunk_frames: int
) -> tuple[dict[str, int], dict[str, int]]:
    blocks: dict[str, int] = {}
    frame_counts: dict[str, int] = {}
    cursor = 0
    for chapter in book.chapters:
        chapter_id = chapter.plan.rule.id
        frames_count = chapter.plan.rule.expected_frames
        completed = max(0, min(frames - cursor, frames_count))
        frame_counts[chapter_id] = completed
        block_count = 0
        if completed:
            first = cursor // chunk_frames
            last = (cursor + completed - 1) // chunk_frames
            block_count = last - first + 1
        blocks[chapter_id] = block_count
        cursor += frames_count
    return blocks, frame_counts


def _update_progress_counts(
    book: ValidatedBook,
    start: int,
    end: int,
    blocks: dict[str, int],
    frames: dict[str, int],
) -> None:
    cursor = 0
    for chapter in book.chapters:
        chapter_id = chapter.plan.rule.id
        chapter_frames = chapter.plan.rule.expected_frames
        chapter_end = cursor + chapter_frames
        if start < chapter_end and end > cursor:
            blocks[chapter_id] = blocks.get(chapter_id, 0) + 1
            frames[chapter_id] = min(
                chapter_frames,
                max(0, end - cursor),
            )
        cursor = chapter_end


def _complete_report(
    *,
    book: ValidatedBook,
    master_path: Path,
    report_path: Path,
    candidate: Candidate,
    before_chapter: list[float],
    before_full: float,
    recovery: dict[str, Any],
) -> dict[str, Any]:
    rules = book.rules
    master = read_wav(master_path, field_path="master")
    output = master.samples
    master_info = master.info
    after_full = integrated_loudness(
        output,
        rules.sample_rate,
        relative_gate_lu=rules.relative_gate_lu,
    )
    after_true_peak = true_peak_dbtp(output)
    maximum_abs = float(np.max(np.abs(output))) if output.size else 0.0
    peak_linear = 10.0 ** (rules.true_peak_dbtp / 20.0)

    chapters: list[dict[str, Any]] = []
    cursor = 0
    noise_ok = True
    clamp_ok = True
    chapter_limits_ok = True
    package_by_chapter: dict[str, list[dict[str, Any]]] = {}
    for package_summary in book.input_summary:
        package_by_chapter.setdefault(
            str(package_summary["chapter"]), []
        ).append(dict(package_summary))

    for index, chapter in enumerate(book.chapters):
        chapter_id = chapter.plan.rule.id
        frames = chapter.plan.rule.expected_frames
        segment = output[cursor : cursor + frames]
        chapter_after = integrated_loudness(
            segment,
            rules.sample_rate,
            relative_gate_lu=rules.relative_gate_lu,
        )
        chapter_true_peak = true_peak_dbtp(segment)
        chapter_sample_peak = sample_peak_dbfs(segment)
        noise_before = noise_floor_dbfs(
            chapter.master_samples, rules.sample_rate
        )
        noise_after = noise_floor_dbfs(segment, rules.sample_rate)
        noise_rise = noise_after - noise_before
        noise_ok = noise_ok and (
            noise_rise <= rules.max_noise_floor_rise_db + 1e-6
        )
        chapter_limits_ok = chapter_limits_ok and (
            chapter_true_peak <= rules.true_peak_dbtp + 1e-6
            and float(np.max(np.abs(segment))) <= peak_linear + 1e-12
        )
        limiter = candidate.limiter_results[index]
        clamp_ok = clamp_ok and (
            limiter.maximum_safety_clamp_run <= MAX_SAFETY_CLAMP_RUN
        )
        chapter_gain = float(candidate.gains_db[index])
        effective_gain = chapter_after - before_chapter[index]
        chapters.append(
            {
                "id": chapter_id,
                "start_frame": cursor,
                "end_frame": cursor + frames,
                "frames": frames,
                "packages": package_by_chapter.get(chapter_id, []),
                "before_loudness_lufs": _rounded(before_chapter[index]),
                "after_loudness_lufs": _rounded(chapter_after),
                "gain_db": _rounded(chapter_gain),
                "static_gain_db": _rounded(
                    float(candidate.static_gain_db[index])
                ),
                "active_region_gain_db": _rounded(
                    float(candidate.activity_gain_db[index])
                ),
                "book_correction_db": _rounded(
                    candidate.global_correction_db
                ),
                "boundary_trim_db": _rounded(
                    candidate.boundary_trims_db.get(index, 0.0)
                ),
                "effective_gain_db": _rounded(effective_gain),
                "peak_protection_samples": limiter.triggered_samples,
                "limiter_active_samples": limiter.protected_samples,
                "max_continuous_protection_run": limiter.maximum_safety_clamp_run,
                "noise_floor_before_dbfs": _rounded(noise_before),
                "noise_floor_after_dbfs": _rounded(noise_after),
                "noise_floor_rise_db": _rounded(noise_rise),
                "sample_peak_after_dbfs": _rounded(chapter_sample_peak),
                "true_peak_after_dbtp": _rounded(chapter_true_peak),
            }
        )
        cursor += frames

    boundaries: list[dict[str, Any]] = []
    boundary_ok = True
    cursor = 0
    for index in range(len(book.chapters) - 1):
        cursor += book.chapters[index].plan.rule.expected_frames
        right_start = cursor
        left_chapter_start = cursor - book.chapters[index].plan.rule.expected_frames
        right_chapter_end = (
            right_start + book.chapters[index + 1].plan.rule.expected_frames
        )
        left_level, right_level, window = boundary_levels(
            output[left_chapter_start:cursor],
            output[right_start:right_chapter_end],
            rules.sample_rate,
        )
        delta = left_level - right_level
        within = abs(delta) <= rules.boundary_tolerance_db + 1e-6
        boundary_ok = boundary_ok and within
        boundaries.append(
            {
                "left_chapter": book.chapters[index].plan.rule.id,
                "right_chapter": book.chapters[index + 1].plan.rule.id,
                "frame_index": cursor,
                "window_frames": window,
                "left_level_dbfs": _rounded(left_level),
                "right_level_dbfs": _rounded(right_level),
                "delta_db": _rounded(delta),
                "tolerance_db": _rounded(rules.boundary_tolerance_db),
                "within_tolerance": within,
            }
        )

    loudness_deviation = after_full - rules.target_loudness_lufs
    loudness_ok = abs(loudness_deviation) <= LOUDNESS_TOLERANCE_LU
    true_peak_ok = (
        after_true_peak <= rules.true_peak_dbtp + 1e-6
        and maximum_abs <= peak_linear + 1e-12
    )
    length_ok = master_info.frames == sum(
        chapter.plan.rule.expected_frames for chapter in book.chapters
    )
    validation = {
        "full_loudness_reached": loudness_ok,
        "true_peak_within_limit": true_peak_ok,
        "all_chapter_peaks_within_limit": chapter_limits_ok,
        "noise_floor_within_limit": noise_ok,
        "boundary_within_tolerance": boundary_ok,
        "length_exact": length_ok,
        "no_continuous_hard_clipping": clamp_ok,
        "all_checks_passed": all(
            (
                loudness_ok,
                true_peak_ok,
                chapter_limits_ok,
                noise_ok,
                boundary_ok,
                length_ok,
                clamp_ok,
            )
        ),
    }
    return {
        "schema_version": 1,
        "status": "complete",
        "business_code": recovery.get("business_code"),
        "master": {
            "file": display_path(master_path, report_path.parent),
            "sample_rate": master_info.sample_rate,
            "channels": master_info.channels,
            "bit_depth": master_info.bit_depth,
            "frames": master_info.frames,
            "samples": master_info.frames,
            "duration_seconds": master_info.frames / master_info.sample_rate,
            "header_bytes": master_info.header_bytes,
            "data_bytes": master_info.data_bytes,
            "file_bytes": master_info.file_size,
            "sha256": sha256_file(master_path),
        },
        "rules": {
            "target_loudness_lufs": rules.target_loudness_lufs,
            "relative_gate_lu": rules.relative_gate_lu,
            "true_peak_dbtp": rules.true_peak_dbtp,
            "max_noise_floor_rise_db": rules.max_noise_floor_rise_db,
            "boundary_tolerance_db": rules.boundary_tolerance_db,
            "block_frames": rules.block_frames,
        },
        "loudness": {
            "before_lufs": _rounded(before_full),
            "target_lufs": rules.target_loudness_lufs,
            "after_lufs": _rounded(after_full),
            "deviation_lu": _rounded(loudness_deviation),
            "gain_policy": "chapter_alignment_plus_book_correction",
        },
        "chapters": chapters,
        "boundaries": boundaries,
        "peak": {
            "measured_true_peak_dbtp": _rounded(after_true_peak),
            "maximum_absolute_sample": _rounded(maximum_abs),
            "limit_dbtp": rules.true_peak_dbtp,
        },
        "validation": validation,
        "recovery": recovery,
    }


def _interrupted_report(
    *,
    book: ValidatedBook,
    master_path: Path,
    report_path: Path,
    candidate: Candidate,
    before_chapter: list[float],
    before_full: float,
    progress: dict[str, Any],
    chapter_blocks: dict[str, int],
    chapter_frames: dict[str, int],
) -> dict[str, Any]:
    rules = book.rules
    chapter_rows = []
    cursor = 0
    for index, chapter in enumerate(book.chapters):
        chapter_id = chapter.plan.rule.id
        frames = chapter.plan.rule.expected_frames
        chapter_rows.append(
            {
                "id": chapter_id,
                "start_frame": cursor,
                "end_frame": cursor + frames,
                "frames": frames,
                "samples": frames,
                "completed_frames": chapter_frames.get(chapter_id, 0),
                "completed_blocks": chapter_blocks.get(chapter_id, 0),
                "before_loudness_lufs": _rounded(before_chapter[index]),
                "planned_gain_db": _rounded(float(candidate.gains_db[index])),
            }
        )
        cursor += frames
    return {
        "schema_version": 1,
        "status": "interrupted",
        "business_code": "E_INTERRUPTED",
        "master": None,
        "planned_master": {
            "file": display_path(master_path, report_path.parent),
            "sample_rate": rules.sample_rate,
            "channels": book.master_channels,
            "bit_depth": rules.bit_depth,
            "frames": progress["total_frames"],
        },
        "rules": {
            "target_loudness_lufs": rules.target_loudness_lufs,
            "relative_gate_lu": rules.relative_gate_lu,
            "true_peak_dbtp": rules.true_peak_dbtp,
            "max_noise_floor_rise_db": rules.max_noise_floor_rise_db,
            "boundary_tolerance_db": rules.boundary_tolerance_db,
            "block_frames": rules.block_frames,
        },
        "loudness": {
            "before_lufs": _rounded(before_full),
            "target_lufs": rules.target_loudness_lufs,
            "after_lufs": None,
            "deviation_lu": None,
            "gain_policy": "chapter_alignment_plus_book_correction",
        },
        "progress": progress,
        "chapters": chapter_rows,
        "boundaries": [],
        "recovery": progress,
    }


def _recovery_payload(
    resume: ResumeState,
    manager: SnapshotManager,
    *,
    completed_blocks: int,
    completed_frames: int,
    total_blocks: int,
    total_frames: int,
    status: str,
    business_code: str | None,
) -> dict[str, Any]:
    return {
        "source": resume.source,
        "resumed_blocks": resume.resumed_blocks,
        "completed_blocks": completed_blocks,
        "completed_frames": completed_frames,
        "total_blocks": total_blocks,
        "total_frames": total_frames,
        "snapshot_sequence": manager.sequence,
        "snapshot_recovered": resume.snapshot_recovered,
        "business_code": business_code,
        "status": status,
    }


def _rounded(value: float) -> float:
    value = float(value)
    if not math.isfinite(value):
        return DB_FLOOR
    return round(value, 9)
