"""End-to-end acceptance checks over deterministic local fixtures."""

from __future__ import annotations

import importlib.metadata
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable
from unittest import mock

import numpy as np

from .audio_io import AudioData, load_audio, write_wav_atomic
from .config import RepairConfig, SessionConfig, default_config
from .dsp import denoise_segment, reconstruct_without_gain
from .engine import (
    RepairSession,
    RunStatus,
    process_full_recompute,
    process_realtime_with_checkpoint,
)
from .errors import AppError
from .metrics import audio_stats, locate_first_difference
from .samples import generate_deterministic_samples
from .segments import SegmentStateMachine, SegmentStatus, split_segments


@dataclass
class CheckLog:
    checks: list[dict[str, Any]] = field(default_factory=list)

    def add(self, name: str, passed: bool, **details: Any) -> bool:
        payload = {"name": name, "passed": bool(passed), **details}
        self.checks.append(payload)
        if not passed:
            raise AssertionError(f"acceptance check failed: {name}: {details}")
        return bool(passed)


def _packages() -> dict[str, str]:
    versions = {}
    for name in ("numpy", "scipy", "PySide6"):
        try:
            versions[name] = importlib.metadata.version(name)
        except importlib.metadata.PackageNotFoundError:
            versions[name] = "not-installed"
    return versions


def _sha_file(path: Path) -> str:
    import hashlib

    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _expected_error(callable_: Callable[[], Any], code: str) -> AppError:
    try:
        callable_()
    except AppError as exc:
        if exc.code != code:
            raise AssertionError(f"expected {code}, got {exc.code}") from exc
        return exc
    raise AssertionError(f"expected business error {code}, no error was raised")


def _segment_number(sample_index: int, segment_samples: int) -> int:
    return sample_index // segment_samples


def _check_reconstruction(log: CheckLog, boundary_dir: Path) -> dict[str, Any]:
    cases = [
        ("boundary_mono_1000.wav", "hann", 256, 64),
        ("boundary_stereo_tail1.wav", "hamming", 512, 128),
        ("boundary_stereo_tail1.wav", "blackman", 512, 128),
        ("boundary_stereo_tail1.wav", "rectangular", 512, 512),
    ]
    results = []
    for filename, window, frame_size, hop_size in cases:
        audio = load_audio(boundary_dir / filename)
        config = RepairConfig(
            frame_size=frame_size,
            hop_size=hop_size,
            window=window,
            strength=0.0,
        )
        reconstructed = reconstruct_without_gain(audio.samples, config)
        error = float(np.max(np.abs(reconstructed - audio.samples)))
        passed = (
            reconstructed.shape == audio.samples.shape
            and reconstructed.shape[0] == audio.total_samples
            and error <= 1e-10
        )
        result = {
            "file": filename,
            "channels": audio.channels,
            "input_samples": audio.total_samples,
            "output_samples": int(reconstructed.shape[0]),
            "window": window,
            "frame_size": frame_size,
            "hop_size": hop_size,
            "max_abs_error": error,
        }
        results.append(result)
        log.add(f"STFT/ISTFT exact duration: {filename}/{window}", passed, **result)
    return {"cases": results}


def _check_strength_monotonic(log: CheckLog, speech: AudioData) -> dict[str, Any]:
    base = default_config("speech")
    values = []
    previous_rms = None
    for strength in (0.0, 0.5, 1.0, 1.5, 2.0, 2.5, 3.0, 4.0):
        config = RepairConfig(**{**base.canonical_dict(), "strength": strength})
        result = denoise_segment(speech.samples, config)
        rms = float(np.sqrt(np.mean(result.output * result.output)))
        monotonic = previous_rms is None or rms <= previous_rms + 1e-15
        values.append(
            {
                "strength": strength,
                "output_rms": rms,
                "mean_gain": result.mean_gain,
                "attenuation_db": result.attenuation_db,
                "monotonic_non_increasing": monotonic,
            }
        )
        log.add(f"Strength monotonic at {strength}", monotonic, rms=rms)
        previous_rms = rms
    return {"values": values}


def _compare_arrays(
    log: CheckLog,
    label: str,
    left: np.ndarray,
    right: np.ndarray,
    segment_samples: int,
) -> dict[str, Any]:
    difference = locate_first_difference(left, right, segment_samples)
    log.add(label, difference.equal, difference=difference.to_dict())
    if not difference.equal:
        raise AssertionError(json.dumps(difference.to_dict(), indent=2))
    return difference.to_dict()


def _run_three_paths(
    log: CheckLog,
    sample_file: Path,
    profile: str,
    work_dir: Path,
    export_dir: Path,
    log_dir: Path,
) -> dict[str, Any]:
    audio = load_audio(sample_file)
    config = default_config(profile)
    session_config = SessionConfig(
        segment_samples=16384,
        max_workers=4,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    sample_name = sample_file.stem
    sample_work = work_dir / sample_name
    sample_export = export_dir / sample_name
    sample_work.mkdir(parents=True, exist_ok=True)
    sample_export.mkdir(parents=True, exist_ok=True)

    full_output = process_full_recompute(audio, config, session_config)
    full_stats = audio_stats(full_output, audio.sample_rate)
    full_path = sample_export / "full_recompute.wav"
    write_wav_atomic(full_path, full_output, audio.sample_rate)

    realtime_output, realtime_result, realtime_session = process_realtime_with_checkpoint(
        audio,
        config,
        session_config,
        sample_work / "realtime_checkpoint",
        clear_checkpoint=True,
    )
    realtime_stats = audio_stats(realtime_output, audio.sample_rate)
    realtime_path = sample_export / "realtime.wav"
    write_wav_atomic(realtime_path, realtime_output, audio.sample_rate)
    log.add(
        f"{sample_name}: realtime complete",
        realtime_result.complete and realtime_result.status == RunStatus.COMPLETED,
        result=realtime_result.to_dict(),
    )

    resume_checkpoint = sample_work / "resume_checkpoint"
    seed = RepairSession(
        audio,
        config,
        session_config,
        resume_checkpoint,
        clear_checkpoint=True,
    )
    seed_result = seed.run(max_new_segments=5)
    log.add(
        f"{sample_name}: resume seed is partial",
        not seed_result.complete and 0 < seed_result.committed < seed_result.total,
        result=seed_result.to_dict(),
    )
    resumed = RepairSession(audio, config, session_config, resume_checkpoint)
    log.add(
        f"{sample_name}: restart loaded committed prefix",
        resumed.load_info.loaded_count == seed_result.committed
        and resumed.load_info.valid,
        load_info=resumed.load_info.to_dict(),
    )
    resume_result = resumed.run()
    resume_output = resumed.collect_outputs() if resume_result.complete else np.empty((0, audio.channels))
    resume_stats = audio_stats(resume_output, audio.sample_rate)
    resume_path = sample_export / "resume.wav"
    write_wav_atomic(resume_path, resume_output, audio.sample_rate)
    log.add(
        f"{sample_name}: resume completed",
        resume_result.complete and resume_result.status == RunStatus.COMPLETED,
        result=resume_result.to_dict(),
    )

    first_difference = _compare_arrays(
        log,
        f"{sample_name}: realtime == full_recompute",
        realtime_output,
        full_output,
        session_config.segment_samples,
    )
    _compare_arrays(
        log,
        f"{sample_name}: resume == full_recompute",
        resume_output,
        full_output,
        session_config.segment_samples,
    )
    stats_equal = realtime_stats.to_dict() == full_stats.to_dict() == resume_stats.to_dict()
    log.add(
        f"{sample_name}: peak/RMS/LUFS/sha exact",
        stats_equal,
        realtime=realtime_stats.to_dict(),
        full=full_stats.to_dict(),
        resume=resume_stats.to_dict(),
    )
    log.add(
        f"{sample_name}: exact source sample count",
        realtime_output.shape == audio.samples.shape
        and resume_output.shape == audio.samples.shape
        and full_output.shape == audio.samples.shape,
        expected=list(audio.samples.shape),
        realtime=list(realtime_output.shape),
    )

    exported_arrays = [
        load_audio(full_path).samples,
        load_audio(realtime_path).samples,
        load_audio(resume_path).samples,
    ]
    export_same = all(
        np.array_equal(exported_arrays[0], candidate) for candidate in exported_arrays[1:]
    )
    log.add(f"{sample_name}: three exported files sample-identical", export_same)

    difference_probe = np.array(full_output, copy=True)
    probe_segment = min(7, len(split_segments(audio.total_samples, 16384)) - 1)
    probe_index = probe_segment * 16384 + 3
    if probe_index < difference_probe.shape[0]:
        difference_probe[probe_index, 0] += 1e-9
    else:
        difference_probe[0, 0] += 1e-9
        probe_index = 0
        probe_segment = 0
    located = locate_first_difference(
        full_output, difference_probe, session_config.segment_samples
    )
    locator_ok = (
        not located.equal
        and located.first_sample == probe_index
        and located.segment_index == _segment_number(probe_index, 16384)
    )
    log.add(
        f"{sample_name}: first-difference locator",
        locator_ok,
        difference=located.to_dict(),
    )

    result = {
        "sample": sample_file.name,
        "profile": profile,
        "source_samples": audio.total_samples,
        "sample_rate": audio.sample_rate,
        "channels": audio.channels,
        "segments": realtime_result.total,
        "remainder_samples": audio.total_samples % session_config.segment_samples,
        "realtime": realtime_result.to_dict(),
        "resume_seed": seed_result.to_dict(),
        "resume": resume_result.to_dict(),
        "load_info": resumed.load_info.to_dict(),
        "first_difference": first_difference,
        "stats": full_stats.to_dict(),
        "exports": {
            "full": str(full_path),
            "realtime": str(realtime_path),
            "resume": str(resume_path),
            "full_sha256": _sha_file(full_path),
            "realtime_sha256": _sha_file(realtime_path),
            "resume_sha256": _sha_file(resume_path),
        },
        "realtime_checkpoint": realtime_session.diagnostics(),
    }
    (log_dir / f"{sample_name}_three_paths.json").write_text(
        json.dumps(result, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    return result


def _crash_and_resume(
    log: CheckLog,
    sample_file: Path,
    work_dir: Path,
    full_output: np.ndarray,
) -> dict[str, Any]:
    audio = load_audio(sample_file)
    checkpoint = work_dir / "crash_checkpoint"
    if checkpoint.exists():
        shutil.rmtree(checkpoint)
    helper = Path(__file__).resolve().parents[2] / "scripts" / "_crash_after_commits.py"
    process = subprocess.run(
        [
            sys.executable,
            str(helper),
            str(sample_file),
            str(checkpoint),
            "16384",
            "4",
        ],
        cwd=str(Path(__file__).resolve().parents[2]),
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )
    log.add(
        "Hard-exit worker terminated without completion report",
        process.returncode == 27,
        returncode=process.returncode,
        stdout=process.stdout,
        stderr=process.stderr,
    )
    config = default_config("speech" if "speech" in sample_file.name else "music")
    session_config = SessionConfig(
        segment_samples=16384,
        max_workers=2,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    resumed = RepairSession(audio, config, session_config, checkpoint)
    resumed_count = resumed.load_info.loaded_count
    log.add(
        "Crash recovery used persisted segments only",
        resumed_count == 4 and resumed.load_info.valid,
        load_info=resumed.load_info.to_dict(),
    )
    result = resumed.run()
    output = resumed.collect_outputs()
    _compare_arrays(
        log,
        "Crash-resume output == full recompute",
        output,
        full_output,
        session_config.segment_samples,
    )
    return {
        "subprocess_returncode": process.returncode,
        "resumed_count": resumed_count,
        "result": result.to_dict(),
        "load_info": resumed.load_info.to_dict(),
    }


def _version_invalidation(
    log: CheckLog,
    sample_file: Path,
    work_dir: Path,
    full_output: np.ndarray,
) -> dict[str, Any]:
    audio = load_audio(sample_file)
    config = default_config("speech")
    session_config = SessionConfig(
        segment_samples=16384,
        max_workers=2,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    checkpoint = work_dir / "version_checkpoint"
    seed = RepairSession(
        audio, config, session_config, checkpoint, clear_checkpoint=True
    )
    seed.run(max_new_segments=3)
    manifest_path = checkpoint / "snapshot" / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    manifest["schema_version"] = 999
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    resumed = RepairSession(audio, config, session_config, checkpoint)
    states = resumed.state_snapshot()
    all_pending = all(record.status == SegmentStatus.PENDING for record in states.values())
    invalidated_dir = (
        Path(resumed.load_info.invalidated_to)
        if resumed.load_info.invalidated_to
        else None
    )
    passed = (
        resumed.load_info.state == "invalidated"
        and resumed.load_info.loaded_count == 0
        and all_pending
        and invalidated_dir is not None
        and invalidated_dir.exists()
        and (invalidated_dir / "INVALIDATION_REASON.txt").exists()
    )
    log.add(
        "Snapshot version mismatch invalidates the whole snapshot",
        passed,
        load_info=resumed.load_info.to_dict(),
        all_pending=all_pending,
    )
    result = resumed.run()
    output = resumed.collect_outputs()
    _compare_arrays(
        log,
        "Post-invalidation recompute matches full recompute",
        output,
        full_output,
        session_config.segment_samples,
    )
    return {
        "load_info": resumed.load_info.to_dict(),
        "all_pending_after_invalidation": all_pending,
        "recomputed_result": result.to_dict(),
    }


def _state_and_error_checks(
    log: CheckLog, boundary_dir: Path, work_dir: Path
) -> dict[str, Any]:
    specs = split_segments(101, 40)
    machine = SegmentStateMachine(specs)
    machine.transition(0, SegmentStatus.PROCESSING)
    illegal = _expected_error(
        lambda: machine.transition(0, SegmentStatus.PENDING), "E_STATE"
    )
    unchanged = machine.get(0).status == SegmentStatus.PROCESSING
    log.add(
        "Illegal transition returns E_STATE and preserves state",
        unchanged,
        error=illegal.to_dict(),
        current=machine.get(0).status.value,
    )
    machine.transition(0, SegmentStatus.FAILED)
    machine.retry(0)
    machine.transition(0, SegmentStatus.COMMITTED)
    retry_ok = machine.get(0).status == SegmentStatus.COMMITTED
    log.add("Failed segment retry transition is legal", retry_ok)

    strength_error = _expected_error(
        lambda: RepairConfig(strength=9.0).validate(), "E_PARAM"
    )
    worker_error = _expected_error(
        lambda: SessionConfig(max_workers=99).validate(1), "E_PARAM"
    )
    memory_error = _expected_error(
        lambda: SessionConfig(
            segment_samples=8_000_000,
            max_workers=8,
            max_buffer_bytes=4 * 1024 * 1024,
        ).validate(2),
        "E_PARAM",
    )
    log.add(
        "Parameter bounds and memory budget return E_PARAM",
        True,
        strength=strength_error.to_dict(),
        workers=worker_error.to_dict(),
        memory=memory_error.to_dict(),
    )

    corrupt = work_dir / "corrupt_input.wav"
    corrupt.write_bytes(b"this is not a wave file\x00\x01" * 20)
    decode = _expected_error(lambda: load_audio(corrupt), "E_DECODE")
    missing = _expected_error(lambda: load_audio(work_dir / "missing.wav"), "E_DECODE")
    log.add(
        "Corrupt and missing audio return E_DECODE",
        True,
        corrupt=decode.to_dict(),
        missing=missing.to_dict(),
    )
    return {
        "illegal_transition": illegal.to_dict(),
        "strength": strength_error.to_dict(),
        "workers": worker_error.to_dict(),
        "memory": memory_error.to_dict(),
        "decode": decode.to_dict(),
    }


def _failure_retry(
    log: CheckLog,
    sample_file: Path,
    work_dir: Path,
    full_output: np.ndarray,
) -> dict[str, Any]:
    audio = load_audio(sample_file)
    config = default_config("music")
    session_config = SessionConfig(
        segment_samples=4096,
        max_workers=2,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    checkpoint = work_dir / "failure_checkpoint"
    import audio_repair.engine as engine_module

    original = engine_module.process_segment
    failed_once = False

    def flaky(audio_arg: AudioData, spec: Any, config_arg: RepairConfig) -> Any:
        nonlocal failed_once
        if spec.index == 1 and not failed_once:
            failed_once = True
            raise RuntimeError("injected deterministic segment fault")
        return original(audio_arg, spec, config_arg)

    with mock.patch.object(engine_module, "process_segment", side_effect=flaky):
        session = RepairSession(
            audio, config, session_config, checkpoint, clear_checkpoint=True
        )
        first = session.run()
        collect_state_error = _expected_error(session.collect_outputs, "E_STATE")
    log.add(
        "Single segment fault becomes FAILED and blocks incomplete export",
        first.failed_indices == (1,)
        and first.committed == first.total - 1
        and collect_state_error.code == "E_STATE",
        result=first.to_dict(),
        collect_error=collect_state_error.to_dict(),
    )
    second = session.run(retry_failed=True)
    output = session.collect_outputs()
    log.add(
        "Failed segment retry completes with same output",
        second.complete and second.status == RunStatus.COMPLETED,
        result=second.to_dict(),
    )
    _compare_arrays(
        log,
        "Failure/retry output == full recompute",
        output,
        full_output,
        session_config.segment_samples,
    )
    return {
        "first_result": first.to_dict(),
        "retry_result": second.to_dict(),
        "collect_error": collect_state_error.to_dict(),
    }


def _pause_resume(
    log: CheckLog,
    sample_file: Path,
    work_dir: Path,
    full_output: np.ndarray,
) -> dict[str, Any]:
    audio = load_audio(sample_file)
    config = default_config("speech")
    session_config = SessionConfig(
        segment_samples=16384,
        max_workers=2,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    checkpoint = work_dir / "pause_checkpoint"
    pause_requested = False
    paused_committed = 0
    holder: dict[str, Any] = {}

    def callback(event: Any) -> None:
        nonlocal pause_requested, paused_committed
        if event.status == RunStatus.RUNNING and event.committed >= 2 and not pause_requested:
            pause_requested = True
            paused_committed = event.committed
            holder["session"].request_pause()

    session = RepairSession(
        audio,
        config,
        session_config,
        checkpoint,
        clear_checkpoint=True,
        progress_callback=callback,
    )
    holder["session"] = session

    def runner() -> None:
        holder["result"] = session.run()

    thread = threading.Thread(target=runner, daemon=True)
    thread.start()
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if session.status == RunStatus.PAUSED:
            break
        time.sleep(0.01)
    was_paused = session.status == RunStatus.PAUSED
    committed_at_pause = session.state_machine.committed_count()
    processing_at_pause = session.state_machine.indexes_with(
        {SegmentStatus.PROCESSING}
    )
    log.add(
        "Pause happens at a committed segment boundary",
        was_paused and 0 < committed_at_pause < session.state_machine.total_count,
        committed=committed_at_pause,
        total=session.state_machine.total_count,
        processing=processing_at_pause,
    )
    session.request_resume()
    thread.join(timeout=60)
    output = session.collect_outputs()
    result = holder.get("result")
    log.add(
        "Paused job resumes and completes",
        result is not None and result.complete,
        result=result.to_dict() if result is not None else None,
    )
    _compare_arrays(
        log,
        "Pause/resume output == full recompute",
        output,
        full_output,
        session_config.segment_samples,
    )
    return {
        "paused_committed": committed_at_pause,
        "processing_at_pause": processing_at_pause,
        "result": result.to_dict() if result is not None else None,
    }


def _config_immutability(
    log: CheckLog, sample_file: Path, work_dir: Path
) -> dict[str, Any]:
    audio = load_audio(sample_file)
    config = default_config("speech")
    session_config = SessionConfig(
        segment_samples=16384,
        max_workers=1,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    checkpoint = work_dir / "config_checkpoint"
    session = RepairSession(
        audio, config, session_config, checkpoint, clear_checkpoint=True
    )
    session.run(max_new_segments=2)
    before = session.store.load_segment(0)
    before_sha = before.output_sha256
    before_strength = before.config.strength
    session.set_repair_config(config.with_strength(0.5))
    session.run()
    after = session.store.load_segment(0)
    log.add(
        "Config change affects only uncommitted segments",
        after.output_sha256 == before_sha
        and after.config.strength == before_strength
        and after.metadata["config_fingerprint"] == before.config_fingerprint,
        before_strength=before_strength,
        after_strength=after.config.strength,
        before_sha=before_sha,
        after_sha=after.output_sha256,
    )
    return {
        "first_segment_config_fingerprint": before.config_fingerprint,
        "first_segment_output_sha256": before_sha,
        "new_config_strength": 0.5,
    }


def _serial_commit_order(log: CheckLog, sample_file: Path, work_dir: Path) -> dict[str, Any]:
    audio = load_audio(sample_file)
    config = default_config("music")
    session_config = SessionConfig(
        segment_samples=4096,
        max_workers=4,
        max_buffer_bytes=64 * 1024 * 1024,
    )
    session = RepairSession(
        audio,
        config,
        session_config,
        work_dir / "order_checkpoint",
        clear_checkpoint=True,
    )
    save_order: list[int] = []
    active = 0
    max_active = 0
    guard = threading.Lock()
    original = session.store.save_segment

    def tracked(spec: Any, output: np.ndarray, config_arg: RepairConfig, dsp_details: Any = None) -> str:
        nonlocal active, max_active
        with guard:
            active += 1
            max_active = max(max_active, active)
        try:
            time.sleep(0.002)
            digest = original(spec, output, config_arg, dsp_details=dsp_details)
            with guard:
                save_order.append(spec.index)
            return digest
        finally:
            with guard:
                active -= 1

    session.store.save_segment = tracked  # type: ignore[method-assign]
    result = session.run()
    log.add(
        "Concurrent workers commit checkpoints serially in ascending order",
        result.complete and max_active == 1 and save_order == sorted(save_order),
        max_active=max_active,
        save_order=save_order,
        result=result.to_dict(),
    )
    return {
        "max_concurrent_checkpoint_writes": max_active,
        "save_order": save_order,
        "result": result.to_dict(),
    }


def _determinism_check(log: CheckLog, sample_dir: Path, work_dir: Path) -> dict[str, Any]:
    manifest = json.loads((sample_dir / "manifest.json").read_text(encoding="utf-8"))
    actual = []
    for record in manifest["samples"]:
        path = sample_dir / record["file"]
        actual.append(
            {
                "file": record["file"],
                "expected_sha256": record["sha256"],
                "actual_sha256": _sha_file(path),
            }
        )
    initial_ok = all(item["expected_sha256"] == item["actual_sha256"] for item in actual)
    regenerated_dir = work_dir / "regenerated_samples"
    regenerated = generate_deterministic_samples(regenerated_dir)
    regen_ok = all(
        expected["sha256"] == produced["sha256"]
        for expected, produced in zip(
            manifest["samples"], regenerated["samples"], strict=True
        )
    )
    log.add(
        "Fixture files match manifest and regenerate byte-identically",
        initial_ok and regen_ok,
        files=actual,
        regenerated_manifest=str(regenerated_dir / "manifest.json"),
    )
    return {
        "manifest_matches": initial_ok,
        "regeneration_matches": regen_ok,
        "files": actual,
    }


def run_acceptance(
    sample_dir: str | os.PathLike[str],
    work_dir: str | os.PathLike[str],
    export_dir: str | os.PathLike[str],
    *,
    report_path: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    sample_root = Path(sample_dir).resolve()
    work_root = Path(work_dir).resolve()
    export_root = Path(export_dir).resolve()
    if work_root.exists():
        shutil.rmtree(work_root)
    if export_root.exists():
        shutil.rmtree(export_root)
    work_root.mkdir(parents=True, exist_ok=True)
    export_root.mkdir(parents=True, exist_ok=True)
    log_root = work_root / "logs"
    log_root.mkdir(parents=True, exist_ok=True)

    log = CheckLog()
    report: dict[str, Any] = {
        "ok": False,
        "format": "audio-repair-acceptance-v1",
        "environment": {
            "platform": platform.platform(),
            "system": platform.system(),
            "python": sys.version,
            "python_executable": sys.executable,
            "packages": _packages(),
        },
        "paths": {
            "samples": str(sample_root),
            "work": str(work_root),
            "exports": str(export_root),
        },
        "checks": log.checks,
    }
    try:
        report["sample_determinism"] = _determinism_check(log, sample_root, work_root)
        report["reconstruction"] = _check_reconstruction(log, sample_root)

        music = load_audio(sample_root / "music_noisy_stereo.wav")
        speech = load_audio(sample_root / "speech_noisy_mono.wav")
        report["strength_monotonic"] = _check_strength_monotonic(log, speech)
        report["paths"] = {
            **report["paths"],
            "music": _run_three_paths(
                log,
                sample_root / "music_noisy_stereo.wav",
                "music",
                work_root,
                export_root,
                log_root,
            ),
            "speech": _run_three_paths(
                log,
                sample_root / "speech_noisy_mono.wav",
                "speech",
                work_root,
                export_root,
                log_root,
            ),
        }

        speech_config = default_config("speech")
        speech_session_config = SessionConfig(
            segment_samples=16384,
            max_workers=4,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        speech_full = process_full_recompute(
            speech, speech_config, speech_session_config
        )
        report["crash_recovery"] = _crash_and_resume(
            log, sample_root / "speech_noisy_mono.wav", work_root, speech_full
        )
        report["version_invalidation"] = _version_invalidation(
            log, sample_root / "speech_noisy_mono.wav", work_root, speech_full
        )
        report["pause_resume"] = _pause_resume(
            log, sample_root / "speech_noisy_mono.wav", work_root, speech_full
        )
        edge = load_audio(sample_root / "boundary_stereo_tail1.wav")
        edge_config = default_config("music")
        edge_session_config = SessionConfig(
            segment_samples=4096,
            max_workers=2,
            max_buffer_bytes=64 * 1024 * 1024,
        )
        edge_full = process_full_recompute(edge, edge_config, edge_session_config)
        report["failure_retry"] = _failure_retry(
            log, sample_root / "boundary_stereo_tail1.wav", work_root, edge_full
        )
        report["state_and_errors"] = _state_and_error_checks(
            log, sample_root, work_root
        )
        report["config_immutability"] = _config_immutability(
            log, sample_root / "speech_noisy_mono.wav", work_root
        )
        report["serial_commit_order"] = _serial_commit_order(
            log, sample_root / "boundary_stereo_tail1.wav", work_root
        )
        report["summary"] = {
            "checks_total": len(log.checks),
            "checks_passed": sum(item["passed"] for item in log.checks),
            "all_passed": all(item["passed"] for item in log.checks),
        }
        report["ok"] = report["summary"]["all_passed"]
    except Exception as exc:  # noqa: BLE001 - acceptance report must capture failures
        report["ok"] = False
        report["failure"] = {
            "type": type(exc).__name__,
            "message": str(exc),
        }
        report["summary"] = {
            "checks_total": len(log.checks),
            "checks_passed": sum(item["passed"] for item in log.checks),
            "all_passed": False,
        }

    if report_path is None:
        report_path = work_root / "acceptance_report.json"
    report_file = Path(report_path)
    report_file.parent.mkdir(parents=True, exist_ok=True)
    report_file.write_text(
        json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    report["report_path"] = str(report_file)
    return report


def render_markdown_summary(report: dict[str, Any]) -> str:
    lines = [
        "# Audio Repair Acceptance Summary",
        "",
        f"- Result: **{'PASS' if report.get('ok') else 'FAIL'}**",
        f"- Python: `{report.get('environment', {}).get('python', '')}`",
        f"- Packages: `{json.dumps(report.get('environment', {}).get('packages', {}), sort_keys=True)}`",
        f"- Checks: {report.get('summary', {}).get('checks_passed', 0)}/"
        f"{report.get('summary', {}).get('checks_total', 0)} passed",
        "",
        "## Three-path comparisons",
        "",
    ]
    paths = report.get("paths", {})
    for key in ("music", "speech"):
        item = paths.get(key)
        if not item:
            continue
        lines.extend(
            [
                f"### {item['sample']}",
                "",
                f"- Samples: {item['source_samples']} ({item['channels']} ch, {item['sample_rate']} Hz)",
                f"- Segments: {item['segments']} (tail {item['remainder_samples']} samples)",
                f"- First difference: `{json.dumps(item['first_difference'], sort_keys=True)}`",
                f"- Output SHA-256: `{item['stats']['sha256']}`",
                f"- Peak: {item['stats']['peak_abs']} ({item['stats']['peak_dbfs']} dBFS)",
                f"- RMS: {item['stats']['rms']} ({item['stats']['rms_dbfs']} dBFS)",
                f"- Integrated LUFS: {item['stats']['integrated_lufs']}",
                "",
            ]
        )
    lines.extend(
        [
            "## Recovery and faults",
            "",
            f"- Crash worker return code: {report.get('crash_recovery', {}).get('subprocess_returncode')}",
            f"- Crash-resumed committed segments: {report.get('crash_recovery', {}).get('resumed_count')}",
            f"- Version invalidation state: {report.get('version_invalidation', {}).get('load_info', {}).get('state')}",
            f"- Failed-segment retry: {report.get('failure_retry', {}).get('retry_result', {}).get('status')}",
            f"- Serial checkpoint writes (max concurrent): "
            f"{report.get('serial_commit_order', {}).get('max_concurrent_checkpoint_writes')}",
            "",
        ]
    )
    return "\n".join(lines)

