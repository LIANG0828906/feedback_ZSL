"""End-to-end render pipeline with block snapshots and crash resume."""

from __future__ import annotations

import json
import os
import signal
import time
import shutil

import numpy as np

from . import (
    PART_BANGU,
    PART_BANGQIANG,
    PART_BED,
    PART_DALUO,
    PART_XIAOLUO,
)
from .align import apply_offsets, compute_offsets
from .cue import validate_wavs
from .decouple import (
    peak_loss_db,
    run_decoupling,
    tail_energy_loss_db,
)
from .dsp import design_oversample_filter
from .errors import ResumeMismatchError
from .loudness import integrated_loudness, section_metrics
from .mastering import comply
from .mix import mix_master
from .snapshot import (
    committed_byte_offset,
    discard_progress,
    read_snapshot,
    snapshot_paths,
    verify_resume,
    write_snapshot,
)
from .wavio import (
    append_wav_block,
    finalize_wav,
    wav_data_offset,
    write_wav_header,
)

EXIT_OK = 0
EXIT_INTERRUPTED = 0


def _round_report(obj, digits=6):
    if isinstance(obj, dict):
        return {k: _round_report(v, digits) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [_round_report(v, digits) for v in obj]
    if isinstance(obj, (np.floating, float)):
        v = float(obj)
        if not np.isfinite(v):
            return None
        return round(v, digits)
    if isinstance(obj, (np.integer,)):
        return int(obj)
    if isinstance(obj, np.ndarray):
        return _round_report(obj.tolist(), digits)
    if isinstance(obj, bool):
        return bool(obj)
    return obj


def run_analysis(cue, base_dir=None, disabled_detection=()):
    """All state that must be frozen before block rendering begins."""
    from .wavio import read_wav

    validate_wavs(cue, base_dir)
    signals = {}
    for ch in cue.channels:
        path = ch.path
        if base_dir and not os.path.isabs(path):
            path = os.path.join(base_dir, path)
        samples, sr, bits, channels = read_wav(path)
        signals[ch.part] = samples[:, 0]

    originals = {k: v.copy() for k, v in signals.items()}
    # Onset detection runs on the raw recordings: their attacks have not yet
    # been attenuated and the declared jitter is what must be corrected.
    offsets, detection_warnings, alignment_detail, micro_shifts = compute_offsets(
        signals, cue, disabled_parts=disabled_detection
    )
    decoupled, decouple_reports = run_decoupling(signals, cue)

    # Injury budgets required by the brief.
    injury = {
        PART_BANGQIANG: {
            "tail_energy_loss_db": tail_energy_loss_db(
                originals[PART_BANGQIANG],
                decoupled[PART_BANGQIANG],
                cue.sample_rate,
                max(cue.reference_times[0] + 0.6, 0.0),
            )
        },
        PART_DALUO: {
            "transient_peak_loss_db": peak_loss_db(
                originals[PART_DALUO],
                decoupled[PART_DALUO],
                cue.sample_rate,
                cue.reference_times,
            )
        },
        PART_BANGU: {
            "transient_peak_loss_db": peak_loss_db(
                originals[PART_BANGU],
                decoupled[PART_BANGU],
                cue.sample_rate,
                cue.reference_times,
            )
        },
    }
    aligned = apply_offsets(decoupled, offsets, micro_shifts=micro_shifts)
    mixed = mix_master(aligned, cue)
    master, compliance = comply(
        mixed,
        cue.sample_rate,
        cue.target_lufs,
        cue.true_peak_limit_db,
    )
    kernel = design_oversample_filter()
    sections = section_metrics(master, cue.sample_rate, cue.sections, kernel)
    overall_lufs = integrated_loudness(master, cue.sample_rate)
    return {
        "master": master,
        "decoupling": decouple_reports,
        "injury": injury,
        "offsets": offsets,
        "micro_shifts": {
            k: [list(pair) for pair in v] for k, v in micro_shifts.items()
        },
        "detection_warnings": detection_warnings,
        "alignment": alignment_detail,
        "compliance": compliance,
        "sections": sections,
        "overall_lufs": overall_lufs,
    }


def _analysis_fingerprint(analysis):
    """Snapshot-safe scalars (the rendered master itself is stored as WAV)."""
    return {
        "decoupling": analysis["decoupling"],
        "offsets": {k: int(v) for k, v in analysis["offsets"].items()},
        "micro_shifts": analysis.get("micro_shifts", {}),
        "limiter": {
            "limiter_triggered_samples": analysis["compliance"][
                "limiter_triggered_samples"
            ],
            "limiter_max_attenuation_db": analysis["compliance"][
                "limiter_max_attenuation_db"
            ],
            "static_trim_db": analysis["compliance"]["static_trim_db"],
        },
    }


class InterruptController:
    """Translates SIGINT into a cooperative flag checked at block borders."""

    def __init__(self):
        self.requested = False
        self._previous = None

    def install(self):
        try:
            self._previous = signal.signal(signal.SIGINT, self._handler)
        except (ValueError, AttributeError):  # not on main thread / Windows edge
            self._previous = None

    def restore(self):
        if self._previous is not None:
            signal.signal(signal.SIGINT, self._previous)
            self._previous = None

    def _handler(self, signum, frame):
        self.requested = True


def _block_count(total, block_size):
    return int(np.ceil(total / block_size))


def render(cue, base_dir=None, disabled_detection=(), interrupt=None,
           start_time=None):
    """Deterministic block renderer with snapshot-based resume."""
    start_time = start_time or time.perf_counter()
    snapshot_dir = (
        cue.snapshot_dir
        if os.path.isabs(cue.snapshot_dir) or base_dir is None
        else os.path.join(base_dir, cue.snapshot_dir)
    )
    paths = snapshot_paths(snapshot_dir)
    os.makedirs(snapshot_dir, exist_ok=True)
    out_dir = os.path.dirname(os.path.abspath(
        os.path.join(base_dir or ".", cue.output_master)
    ))
    os.makedirs(out_dir, exist_ok=True)

    existing = read_snapshot(snapshot_dir)
    if existing is not None:
        # Analysis must be reproducible; compare against a fresh analysis.
        analysis = run_analysis(cue, base_dir, disabled_detection)
        fingerprint = _analysis_fingerprint(analysis)
        try:
            verify_resume(existing, cue, fingerprint)
        except ResumeMismatchError:
            discard_progress(snapshot_dir)
            raise
        master = analysis["master"]
        resume_blocks = int(existing["completed_blocks"])
        resumed = True
    else:
        analysis = run_analysis(cue, base_dir, disabled_detection)
        fingerprint = _analysis_fingerprint(analysis)
        master = analysis["master"]
        resume_blocks = 0
        resumed = False

    n = master.shape[0]
    blocks = _block_count(n, cue.block_samples)
    bits = cue.bit_depth
    master_path = os.path.join(base_dir or ".", cue.output_master)
    partial_path = paths["master"]

    if resume_blocks == 0:
        write_wav_header(partial_path, cue.sample_rate, 2, bits)
    byte_cursor = committed_byte_offset(
        {"completed_blocks": resume_blocks, "block_samples": cue.block_samples},
        channels=2,
        bits=bits,
    )

    completed = resume_blocks
    interrupted_cleanly = False
    kill_after = os.environ.get("OPERA_KILL_AFTER_BLOCK")
    interrupt_after = os.environ.get("OPERA_INTERRUPT_AFTER_BLOCK")
    for block_index in range(resume_blocks, blocks):
        start = block_index * cue.block_samples
        end = min(n, start + cue.block_samples)
        frame = master[start:end]
        offset = wav_data_offset(2, bits) + byte_cursor
        written = append_wav_block(
            partial_path, frame, cue.sample_rate, 2, bits, offset
        )
        byte_cursor += written
        completed = block_index + 1
        payload = {
            "completed_blocks": completed,
            "total_blocks": blocks,
            "block_samples": cue.block_samples,
            "total_samples": n,
            "cue_digest": cue.digest(),
            "cue_summary": cue.canonical_dict(),
            "output_master": cue.output_master,
            **fingerprint,
        }
        write_snapshot(snapshot_dir, payload)
        # Deterministic test hooks (no effect in normal operation).
        if kill_after is not None and completed == int(kill_after):
            os._exit(137)
        if interrupt_after is not None and completed == int(interrupt_after):
            if interrupt is not None:
                interrupt.requested = True
        if interrupt is not None and interrupt.requested:
            interrupted_cleanly = True
            break

    if interrupted_cleanly and completed < blocks:
        finalize_wav(partial_path)
        recovery = {
            "status": "INTERRUPTED",
            "exit_code": 0,
            "completed_blocks": completed,
            "total_blocks": blocks,
            "snapshot_dir": snapshot_dir,
            "resume_hint": "re-run the same render command to continue",
        }
        _write_json(
            os.path.join(snapshot_dir, "recovery_report.json"), recovery
        )
        if interrupt is not None:
            interrupt.restore()
        return {"interrupted": True, "completed_blocks": completed,
                "total_blocks": blocks, "analysis": analysis}

    # Successful run: publish the master and write the full quality report.
    finalize_wav(partial_path)
    _publish_file(partial_path, master_path)
    report = build_report(
        cue, analysis, completed, blocks, resumed, start_time,
        interrupted_cleanly=False,
    )
    report_path = os.path.join(base_dir or ".", cue.output_report)
    os.makedirs(os.path.dirname(os.path.abspath(report_path)), exist_ok=True)
    _write_json(report_path, report)
    discard_progress(snapshot_dir)
    if interrupt is not None:
        interrupt.restore()
    return {"interrupted": False, "report": report, "analysis": analysis}


def build_report(cue, analysis, completed_blocks, total_blocks, resumed,
                 start_time, interrupted_cleanly):
    elapsed = round(time.perf_counter() - start_time, 6)
    report = {
        "schema_version": 1,
        "cue_digest": cue.digest(),
        "sample_rate": cue.sample_rate,
        "bit_depth": cue.bit_depth,
        "total_samples": analysis["master"].shape[0],
        "block_samples": cue.block_samples,
        "completed_blocks": completed_blocks,
        "total_blocks": total_blocks,
        "resumed_from_snapshot": bool(resumed),
        "interrupted_cleanly": bool(interrupted_cleanly),
        "decoupling": {
            "channels": analysis["decoupling"],
            "injury_budgets": analysis["injury"],
        },
        "alignment": {
            "details": analysis["alignment"],
            "offsets_samples": {
                k: int(v) for k, v in analysis["offsets"].items()
            },
            "micro_shifts": analysis.get("micro_shifts", {}),
            "warnings": analysis["detection_warnings"],
        },
        "mastering": analysis["compliance"],
        "section_metrics": analysis["sections"],
        "overall_integrated_lufs": _finite(analysis["overall_lufs"]),
        "elapsed_seconds": elapsed,
    }
    return _round_report(report)


def _finite(v):
    return round(float(v), 6) if np.isfinite(v) else None


def _write_json(path, payload):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(
            _round_report(payload), fh, ensure_ascii=False, sort_keys=True,
            indent=2,
        )
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def _publish_file(src, dst):
    """Move src->dst, falling back to copy+unlink across filesystems."""
    try:
        os.replace(src, dst)
    except OSError as exc:
        if getattr(exc, "errno", None) != 18:  # EXDEV cross-device link
            raise
        shutil.copyfile(src, dst)
        os.remove(src)
