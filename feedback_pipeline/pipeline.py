"""End-to-end orchestration: validate -> trace -> remove -> report -> render.

The stereo bus is rendered in fixed-size sample blocks.  After every block a
snapshot is committed atomically, so a killed process resumes from the last
committed block.  A graceful Ctrl+C finishes the current block, commits the
snapshot, writes a recovery report and exits 0.
"""

from __future__ import annotations

import json
import math
import os
import time
from pathlib import Path

import numpy as np
import soundfile as sf

from . import checkpoint, errors
from .detect import analyze_channel, trace_feedback
from .loudness import integrated_loudness, true_peak
from .project import Project, load_project, project_digest
from .remove import narrowband_energy, remove_feedback

BUS_SUBTYPE = "PCM_24"
CHANNEL_SUBTYPE = "PCM_24"


class InterruptFlag:
    """Shared flag set by the SIGINT handler in the CLI layer."""

    def __init__(self) -> None:
        self.requested = False


def _read_channel(project: Project, name: str) -> np.ndarray:
    spec = next(c for c in project.channels if c.name == name)
    data, _ = sf.read(str(project.channel_path(spec)), dtype="float64", always_2d=True)
    return np.ascontiguousarray(data[:, 0])


def _compensated_weights(project: Project) -> dict[str, float]:
    weights = dict(project.downmix_weights)
    total = sum(weights.values())
    if project.downmix_compensation == "normalize" and total > 0 and abs(total - 1.0) > 1e-12:
        weights = {k: v / total for k, v in weights.items()}
    return weights


def _process_channels(project: Project, detection: dict, processed_dir: Path,
                      reference: np.ndarray) -> dict:
    """Run feedback removal and write processed channel WAVs. Returns report."""
    processed_dir.mkdir(parents=True, exist_ok=True)
    sr = project.sample_rate
    removal: dict[str, dict] = {}
    for spec in project.mix_channels:
        original = _read_channel(project, spec.name)
        rec = detection["channels"][spec.name]
        if rec["detected"]:
            segments = [list(s) for s in rec["segments_samples"]]
            f_est = float(rec["fundamental_hz"])
            processed, gain = remove_feedback(original, reference, sr, f_est, segments)
            e_before = narrowband_energy(original, sr, f_est, segments)
            e_after = narrowband_energy(processed, sr, f_est, segments)
            attenuation_db = 10.0 * math.log10(max(e_after, 1e-30) / max(e_before, 1e-30))
            residual = analyze_channel(processed, reference, sr,
                                       detection["reference_fundamental_hz"])
            diff = processed - original
            removal[spec.name] = {
                "gain": round(gain, 8),
                "segments_samples": segments,
                "modified_ranges_count": len(segments),
                "modified_samples": int(sum(e - s for s, e in segments)),
                "max_modification": float(np.max(np.abs(diff))) if diff.size else 0.0,
                "narrowband_energy_before": e_before,
                "narrowband_energy_after": e_after,
                "attenuation_db": round(attenuation_db, 3),
                "residual_feedback_detected": bool(residual["detected"]),
                "residual_envelope_correlation": residual["envelope_correlation"],
            }
        else:
            processed = original
        sf.write(str(processed_dir / f"{spec.name}.wav"), processed, sr, subtype=CHANNEL_SUBTYPE)
    return removal


def _load_processed(project: Project, processed_dir: Path) -> dict[str, np.ndarray]:
    data = {}
    for spec in project.mix_channels:
        path = processed_dir / f"{spec.name}.wav"
        ch, _ = sf.read(str(path), dtype="float64", always_2d=True)
        data[spec.name] = np.ascontiguousarray(ch[:, 0])
    return data


def _mixdown(project: Project, channels: dict[str, np.ndarray],
             weights: dict[str, float], n: int) -> np.ndarray:
    mix = np.zeros(n, dtype=np.float64)
    for name, w in weights.items():
        ch = channels[name]
        mix[: len(ch)] += w * ch
    return mix


def _snapshot_payload(digest: str, detection: dict, removal: dict, block_size: int,
                      total_blocks: int, completed: int, raw_bytes: int) -> dict:
    return {
        "version": 1,
        "project_digest": digest,
        "detection": detection,
        "removal": removal,
        "block_size": block_size,
        "total_blocks": total_blocks,
        "completed_blocks": completed,
        "bus_raw_bytes": raw_bytes,
    }


def run_pipeline(project_path: str | Path, out_dir: str | Path, block_size: int | None = None,
                 block_delay_ms: int = 0, interrupt_flag: InterruptFlag | None = None) -> dict:
    started = time.monotonic()
    project = load_project(project_path)
    digest = project_digest(project)
    sr = project.sample_rate
    if block_size is None:
        block_size = project.block_size
    flag = interrupt_flag or InterruptFlag()

    out = Path(out_dir)
    work = out / "work"
    processed_dir = work / "processed"
    raw_path = work / "bus.raw"
    out.mkdir(parents=True, exist_ok=True)

    resume_events: list[dict] = []

    # Detection is deterministic; recompute it so stale snapshots that disagree
    # with the current inputs are detected and discarded.
    mix_names = [c.name for c in project.mix_channels]
    channel_data = {name: _read_channel(project, name) for name in mix_names}
    reference = _read_channel(project, project.reference_channels[0].name)
    detection = trace_feedback(channel_data, reference, sr)

    snapshot = checkpoint.load_latest(work)
    removal: dict[str, dict]
    completed = 0
    if snapshot is not None:
        mismatch = (
            snapshot["project_digest"] != digest
            or snapshot["detection"] != detection
            or snapshot["block_size"] != block_size
        )
        processed_ok = all((processed_dir / f"{n}.wav").is_file() for n in mix_names)
        if mismatch or not processed_ok:
            resume_events.append({
                "code": errors.E_RESUME_MISMATCH,
                "message": "stored snapshot does not match current project/detection; restarting from zero",
            })
            checkpoint.wipe(work)
            snapshot = None
        else:
            removal = snapshot["removal"]
            completed = int(snapshot["completed_blocks"])
            # Roll the raw bus back to the last committed byte offset.
            with open(raw_path, "r+b") as fh:
                fh.truncate(int(snapshot["bus_raw_bytes"]))

    if snapshot is None:
        checkpoint.wipe(work)
        processed_dir.mkdir(parents=True, exist_ok=True)
        removal = _process_channels(project, detection, processed_dir, reference)
        (work / "detection.json").write_text(
            json.dumps(detection, indent=2, sort_keys=True), encoding="utf-8"
        )
        completed = 0

    processed = _load_processed(project, processed_dir)
    weights = _compensated_weights(project)
    n_total = max(len(ch) for ch in processed.values())

    # Loudness / true-peak pre-pass on the unprocessed-gain mix.
    mix = _mixdown(project, processed, weights, n_total)
    loudness_before = integrated_loudness([mix, mix], sr)
    gain = 10.0 ** ((project.loudness_target_lufs - loudness_before) / 20.0)
    loudness_after = integrated_loudness([mix * gain, mix * gain], sr)
    if abs(loudness_after - project.loudness_target_lufs) > project.loudness_tolerance_lu:
        raise errors.PipelineError(
            errors.BUS_LOUDNESS_OUT_OF_TOLERANCE,
            f"integrated loudness {loudness_after:.3f} LUFS deviates from target "
            f"{project.loudness_target_lufs} by more than {project.loudness_tolerance_lu} LU",
        )
    tp_after = true_peak([mix * gain, mix * gain], sr)
    if tp_after > project.true_peak_ceiling_dbtp:
        raise errors.PipelineError(
            errors.BUS_TRUE_PEAK_EXCEEDED,
            f"true peak {tp_after:.3f} dBTP exceeds ceiling {project.true_peak_ceiling_dbtp} dBTP",
        )

    # Block-wise render of the stereo bus into a raw float64 file.
    total_blocks = math.ceil(n_total / block_size)
    handles = {name: sf.SoundFile(str(processed_dir / f"{name}.wav")) for name in mix_names}
    interrupted = False
    done = completed
    try:
        for h in handles.values():
            h.seek(completed * block_size)
        raw_mode = "r+b" if completed > 0 and raw_path.exists() else "wb"
        with open(raw_path, raw_mode) as raw:
            raw.seek(completed * block_size * 2 * 8)
            for block_idx in range(completed, total_blocks):
                start = block_idx * block_size
                want = min(block_size, n_total - start)
                block = np.zeros(want, dtype=np.float64)
                for name in mix_names:
                    data = handles[name].read(want, dtype="float64", always_2d=True)
                    block[: len(data)] += weights[name] * data[:, 0]
                block *= gain
                stereo = np.empty(want * 2, dtype=np.float64)
                stereo[0::2] = block
                stereo[1::2] = block
                raw.write(stereo.tobytes())
                raw.flush()
                os.fsync(raw.fileno())
                done = block_idx + 1
                raw_bytes = (start + want) * 2 * 8
                checkpoint.commit(work, _snapshot_payload(
                    digest, detection, removal, block_size, total_blocks, done, raw_bytes))
                if block_delay_ms:
                    time.sleep(block_delay_ms / 1000.0)
                if flag.requested:
                    interrupted = True
                    break
    finally:
        for h in handles.values():
            h.close()

    if interrupted:
        recovery = {
            "status": "interrupted",
            "completed_blocks": done,
            "total_blocks": total_blocks,
            "block_size": block_size,
            "snapshot_dir": str(work),
            "resume_command": f"python -m feedback_pipeline run --project {project.path} --out {out}",
        }
        (out / "recovery_report.json").write_text(
            json.dumps(recovery, indent=2, sort_keys=True), encoding="utf-8"
        )
        return {"status": "interrupted", "recovery_report": recovery}

    # Finalise: assemble the stereo WAV from the raw blocks.
    raw = np.fromfile(raw_path, dtype=np.float64)
    stereo = raw.reshape(-1, 2)
    bus_path = out / "bus.wav"
    sf.write(str(bus_path), stereo, sr, subtype=BUS_SUBTYPE)

    contributions = {}
    total_energy = sum(float(np.dot(weights[n] * processed[n], weights[n] * processed[n]))
                       for n in mix_names)
    for name in mix_names:
        e = float(np.dot(weights[name] * processed[name], weights[name] * processed[name]))
        contributions[name] = round(e / total_energy, 6) if total_energy > 0 else 0.0

    report = {
        "status": "completed",
        "project": str(project.path),
        "project_digest": digest,
        "sample_rate": sr,
        "block_size": block_size,
        "total_blocks": total_blocks,
        "detection": detection,
        "removal": removal,
        "bus": {
            "file": str(bus_path),
            "samples": int(n_total),
            "channels": 2,
            "integrated_loudness_lufs": round(loudness_after, 4),
            "loudness_target_lufs": project.loudness_target_lufs,
            "loudness_ok": True,
            "true_peak_dbtp": round(tp_after, 4),
            "true_peak_ceiling_dbtp": project.true_peak_ceiling_dbtp,
            "true_peak_ok": True,
            "normalization_gain_db": round(20.0 * math.log10(gain), 4),
            "contributions": contributions,
        },
        "resume_events": resume_events,
        "elapsed_seconds": round(time.monotonic() - started, 3),
    }
    (out / "report.json").write_text(
        json.dumps(report, indent=2, sort_keys=True), encoding="utf-8"
    )
    return report
