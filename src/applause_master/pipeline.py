"""End-to-end pipeline: detect -> ratio -> render -> loudness -> report."""

from __future__ import annotations

import json
import os

import numpy as np
import soundfile as sf

from .bands import compute_ratios
from .detect import detect_events
from .errors import BusinessError, E_RESUME_MISMATCH
from .loudness import integrated_loudness, true_peak, true_peak_limit
from .render import render_master, stable_hash_bytes
from .synth import derive_schedule
from .wavio import write_wav


def read_microphones(project):
    signals = {}
    total = 0
    for mic in project["microphones"]:
        data, sr = sf.read(mic["file"], dtype="float64", always_2d=True)
        if sr != project["sample_rate"]:
            raise BusinessError("SAMPLE_RATE_MISMATCH", "reload mismatch", {"mic": mic["name"]})
        signals[mic["name"]] = data.mean(axis=1)
        total = max(total, len(signals[mic["name"]]))
    return signals, total


def ground_truth(project):
    return derive_schedule(project["seed"], project["duration_seconds"],
                           project["sample_rate"], project["microphones"])


def detection_accuracy(events, truth, sample_rate):
    """RMSE of detected boundaries vs ground truth, in milliseconds."""
    pairs = min(len(events), len(truth["events"]))
    errors_ms = []
    for i in range(pairs):
        errors_ms.append((events[i]["start"] - truth["events"][i]["start"]) * 1000.0)
        errors_ms.append((events[i]["end"] - truth["events"][i]["end"]) * 1000.0)
    if not errors_ms:
        return None
    return float(np.sqrt(np.mean(np.square(errors_ms))))


def normalize_to_target(master, sample_rate, target_lufs, tp_limit_dbtp, max_passes=4):
    """Bring integrated loudness within +/-0.5 LU while staying under the TP cap."""
    limit_info = None
    for _ in range(max_passes):
        loudness = integrated_loudness(master, sample_rate)
        delta = target_lufs - loudness
        if abs(delta) <= 0.5:
            break
        adjust = 10.0 ** (delta / 20.0)
        candidate = master * adjust
        tp, _ = true_peak(candidate, sample_rate)
        if tp > tp_limit_dbtp:
            if limit_info is None:
                limit_info = {
                    "triggered_samples": 0,
                    "max_attenuation_db": 0.0,
                    "pre_limiter_loudness_lufs": round(loudness, 3),
                    "post_true_peak_dbtp": round(tp, 4),
                }
            candidate, limit_info = true_peak_limit(candidate, sample_rate, tp_limit_dbtp)
        elif limit_info is None:
            limit_info = {
                "triggered_samples": 0,
                "max_attenuation_db": 0.0,
                "pre_limiter_loudness_lufs": round(loudness + delta, 3),
                "post_true_peak_dbtp": round(tp, 4),
            }
        master = candidate
    if limit_info is None:
        tp, _ = true_peak(master, sample_rate)
        limit_info = {
            "triggered_samples": 0,
            "max_attenuation_db": 0.0,
            "pre_limiter_loudness_lufs": round(integrated_loudness(master, sample_rate), 3),
            "post_true_peak_dbtp": round(tp, 4),
        }
    return master, limit_info, loudness


def run_pipeline(project, output_dir, interrupted=None):
    """Execute the full render pipeline and write the stable JSON report."""
    os.makedirs(output_dir, exist_ok=True)
    master_path = os.path.join(output_dir, "master.wav")
    final_path = os.path.join(output_dir, "master_final.wav")
    snapshot_path = os.path.join(output_dir, "snapshot.json")
    report_path = os.path.join(output_dir, "report.json")
    resume_report_path = os.path.join(output_dir, "resume_report.json")

    mic_signals, total_samples = read_microphones(project)
    events = detect_events(mic_signals, project["sample_rate"], project["detection_threshold"])
    ratio_table, _, _ = compute_ratios(
        events,
        [mic["name"] for mic in project["microphones"]],
        project["freq_bands"],
        mic_signals,
        project["sample_rate"],
        project["energy_gate"],
    )

    mismatch_resumed = False
    try:
        _, snapshot, was_interrupted, resume_count = render_master(
            project, events, ratio_table, mic_signals, master_path,
            snapshot_path, interrupted=interrupted,
        )
    except BusinessError as exc:
        if exc.code != E_RESUME_MISMATCH:
            raise
        mismatch_resumed = True
        # Restart from zero using the current input; stale files were removed.
        _, snapshot, was_interrupted, resume_count = render_master(
            project, events, ratio_table, mic_signals, master_path,
            snapshot_path, interrupted=interrupted,
        )

    if was_interrupted:
        resume_report = build_resume_report(snapshot, events, ratio_table, resume_count)
        with open(resume_report_path, "w", encoding="utf-8") as handle:
            json.dump(resume_report, handle, indent=2, sort_keys=False)
        return {"status": "interrupted", "resume_report": resume_report_path,
                "exit_code": 0, "snapshot": snapshot_path}

    raw_master, sr = sf.read(master_path, dtype="float64", always_2d=True)
    assert len(raw_master) == total_samples
    master, limit_info, final_loudness = normalize_to_target(
        raw_master, sr, project["target_loudness_lufs"], project["true_peak_limit_dbtp"],
    )
    final_tp, final_tp_linear = true_peak(master, sr)
    write_wav(final_path, master, sr, subtype="FLOAT")

    truth = ground_truth(project)
    report = build_report(
        project, events, ratio_table, truth, limit_info, final_path,
        final_loudness, final_tp, total_samples, resume_count, mismatch_resumed,
    )
    with open(report_path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, indent=2, sort_keys=False)
    return {"status": "complete", "report": report_path, "master": final_path,
            "exit_code": 0, "snapshot": snapshot_path}


def build_resume_report(snapshot, events, ratio_table, resume_count):
    return {
        "status": "interrupted",
        "completed_blocks": snapshot["completed_blocks"],
        "total_samples": snapshot["total_samples"],
        "resume_count": resume_count,
        "events": events,
        "ratio_table": ratio_table,
        "block_samples": snapshot["block_samples"],
    }


def build_report(project, events, ratio_table, truth, limit_info, final_path,
                 final_loudness, final_tp, total_samples, resume_count, mismatch_resumed):
    with open(final_path, "rb") as handle:
        master_hash = stable_hash_bytes(handle.read())
    return {
        "seed": project["seed"],
        "sample_rate": project["sample_rate"],
        "total_samples": int(total_samples),
        "event_count": len(events),
        "events": events,
        "band_energy_ratios": ratio_table,
        "ground_truth": truth,
        "detection_rmse_ms": detection_accuracy(events, truth, project["sample_rate"]),
        "limiter": limit_info,
        "loudness": {
            "target_lufs": project["target_loudness_lufs"],
            "measured_integrated_lufs": round(final_loudness, 3),
            "true_peak_limit_dbtp": project["true_peak_limit_dbtp"],
            "measured_true_peak_dbtp": round(final_tp, 4),
        },
        "master_sha256": master_hash,
        "resume_count": int(resume_count),
        "resume_mismatch_restart": bool(mismatch_resumed),
    }
