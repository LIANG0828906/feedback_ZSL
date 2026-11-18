from __future__ import annotations

import hashlib
import json
import math
import os
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np
import soundfile as sf

from .dsp import (
    EPS,
    LimiterStats,
    apply_peak_limiter,
    integrated_loudness_lufs,
    midband_energy_ratio,
    seal_loop_endpoints,
    seam_metrics,
    true_peak_dbtp,
)
from .errors import ResumeMismatch
from .manifest import Manifest, pan_gains, pan_theta
from .masking import MaskingPlan, build_masking_plan, duck_envelope
from .synthesis import write_json_atomic
from .wavio import write_float32_wav


ALGORITHM_VERSION = "rainforest-master-v1"
STEREO_NAME = "rainforest_stereo_master.wav"
MONO_NAME = "rainforest_mono_master.wav"
REPORT_NAME = "render_report.json"
RECOVERY_NAME = "recovery_report.json"


@dataclass
class RenderOutcome:
    status: str
    completed_blocks: int
    total_blocks: int
    report: dict[str, Any] | None = None
    report_path: Path | None = None
    interrupted: bool = False


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _read_mono(handle: sf.SoundFile, start: int, frames: int) -> np.ndarray:
    handle.seek(start)
    data = handle.read(frames, dtype="float32", always_2d=False)
    if data.size == 0:
        return np.zeros(0, dtype=np.float32)
    return np.asarray(data, dtype=np.float32).reshape(-1)


def _pan_for_frames(manifest: Manifest, track, indices: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    return pan_gains(track, indices, manifest.frames, manifest.sample_rate)


class Renderer:
    def __init__(
        self,
        manifest: Manifest,
        output_dir: str | Path,
        state_dir: str | Path | None = None,
    ) -> None:
        self.manifest = manifest
        self.output_dir = Path(output_dir).resolve()
        self.output_dir.mkdir(parents=True, exist_ok=True)
        self.state_dir = (
            Path(state_dir).resolve()
            if state_dir is not None
            else self.output_dir / ".render_state"
        )
        self.block_dir = self.state_dir / "blocks"
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.block_dir.mkdir(parents=True, exist_ok=True)
        self.block_frames = manifest.rendering.block_frames
        self.total_blocks = (manifest.frames + self.block_frames - 1) // self.block_frames
        self.plan: MaskingPlan = build_masking_plan(manifest)
        self.plan_digest = self.plan.digest()
        self.snapshot_path = self.state_dir / "snapshot.json"
        self.completed_blocks = 0
        self.committed_blocks: list[dict[str, Any]] = []
        self._handles: dict[str, sf.SoundFile] = {}

    def _open_handles(self) -> None:
        for track in self.manifest.tracks:
            self._handles[track.name] = sf.SoundFile(str(track.path))

    def close(self) -> None:
        for handle in self._handles.values():
            handle.close()
        self._handles.clear()

    def __enter__(self) -> "Renderer":
        self._open_handles()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    def _snapshot_payload(self, completed_blocks: int) -> dict[str, Any]:
        return {
            "state_version": 1,
            "algorithm_version": ALGORITHM_VERSION,
            "manifest_sha256": self.manifest.manifest_sha256,
            "manifest_canonical_sha256": self.manifest.canonical_digest(),
            "decoupling_digest": self.plan_digest,
            "decoupling_parameters": self.plan.digest_payload(),
            "sample_rate": self.manifest.sample_rate,
            "total_frames": self.manifest.frames,
            "crossfade_frames": self.manifest.crossfade_frames,
            "block_frames": self.block_frames,
            "total_blocks": self.total_blocks,
            "completed_blocks": completed_blocks,
            "next_block": completed_blocks,
            "blocks": self.committed_blocks,
        }

    def _discard_state(self) -> None:
        if self.block_dir.exists():
            shutil.rmtree(self.block_dir)
        self.block_dir.mkdir(parents=True, exist_ok=True)
        if self.snapshot_path.exists():
            self.snapshot_path.unlink()
        for temp in self.state_dir.glob("*.tmp"):
            temp.unlink(missing_ok=True)
        self.completed_blocks = 0
        self.committed_blocks = []

    def load_or_initialize(self) -> bool:
        """Return True when a committed snapshot was accepted for resume."""
        if not self.snapshot_path.is_file():
            self._discard_state()
            return False
        try:
            snapshot = json.loads(self.snapshot_path.read_text(encoding="utf-8"))
        except Exception as exc:
            self._discard_state()
            raise ResumeMismatch(f"snapshot is unreadable: {exc}") from exc

        expected = {
            "state_version": 1,
            "algorithm_version": ALGORITHM_VERSION,
            "manifest_sha256": self.manifest.manifest_sha256,
            "decoupling_digest": self.plan_digest,
            "sample_rate": self.manifest.sample_rate,
            "total_frames": self.manifest.frames,
            "crossfade_frames": self.manifest.crossfade_frames,
            "block_frames": self.block_frames,
            "total_blocks": self.total_blocks,
        }
        for key, value in expected.items():
            if snapshot.get(key) != value:
                self._discard_state()
                raise ResumeMismatch(f"snapshot field mismatch: {key}")
        completed = snapshot.get("completed_blocks")
        blocks = snapshot.get("blocks")
        if not isinstance(completed, int) or completed < 0 or completed > self.total_blocks:
            self._discard_state()
            raise ResumeMismatch("snapshot completed_blocks is invalid")
        if not isinstance(blocks, list) or len(blocks) != completed:
            self._discard_state()
            raise ResumeMismatch("snapshot block index is invalid")

        validated: list[dict[str, Any]] = []
        for index, item in enumerate(blocks):
            if not isinstance(item, dict) or item.get("index") != index:
                self._discard_state()
                raise ResumeMismatch(f"snapshot block index mismatch at {index}")
            start = index * self.block_frames
            expected_frames = min(self.block_frames, self.manifest.frames - start)
            if item.get("frames") != expected_frames:
                self._discard_state()
                raise ResumeMismatch(f"snapshot frame count mismatch at block {index}")
            for key, suffix in (("stereo", "stereo"), ("mono", "mono")):
                file_name = item.get(f"{key}_file")
                digest = item.get(f"{key}_sha256")
                if not isinstance(file_name, str) or not isinstance(digest, str):
                    self._discard_state()
                    raise ResumeMismatch(f"snapshot block metadata missing at block {index}")
                file_path = self.state_dir / file_name
                channels = 2 if suffix == "stereo" else 1
                expected_size = expected_frames * channels * 4
                if not file_path.is_file() or file_path.stat().st_size != expected_size:
                    self._discard_state()
                    raise ResumeMismatch(f"committed block payload missing at block {index}")
                if _sha256_file(file_path) != digest:
                    self._discard_state()
                    raise ResumeMismatch(f"committed block hash mismatch at block {index}")
            validated.append(item)
        self.completed_blocks = completed
        self.committed_blocks = validated
        return completed > 0

    def _write_block_file(self, path: Path, array: np.ndarray) -> str:
        temp = path.with_suffix(path.suffix + ".tmp")
        with temp.open("wb") as handle:
            array.astype("<f4", copy=False).tofile(handle)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
        digest = _sha256_file(path)
        return digest

    def _commit_block(self, index: int, stereo: np.ndarray, mono: np.ndarray) -> None:
        stereo_name = f"blocks/{index:05d}.stereo.f32"
        mono_name = f"blocks/{index:05d}.mono.f32"
        stereo_path = self.state_dir / stereo_name
        mono_path = self.state_dir / mono_name
        stereo_hash = self._write_block_file(stereo_path, stereo)
        mono_hash = self._write_block_file(mono_path, mono)
        start = index * self.block_frames
        self.committed_blocks.append(
            {
                "index": index,
                "start": start,
                "frames": int(stereo.shape[0]),
                "stereo_file": stereo_name,
                "stereo_sha256": stereo_hash,
                "mono_file": mono_name,
                "mono_sha256": mono_hash,
            }
        )
        self.completed_blocks = index + 1
        write_json_atomic(self.snapshot_path, self._snapshot_payload(self.completed_blocks))

    def _render_mix_block(
        self,
        start: int,
        end: int,
        *,
        crossfade: bool = True,
        apply_duck: bool = True,
    ) -> np.ndarray:
        frames = end - start
        output = np.zeros((frames, 2), dtype=np.float64)
        crossfade_frames = self.manifest.crossfade_frames
        head_count = min(frames, max(0, crossfade_frames - start)) if crossfade else 0
        duck_gain = (
            duck_envelope(start, end, self.plan.windows) if apply_duck else np.ones(frames, dtype=np.float32)
        )

        for track in self.manifest.tracks:
            handle = self._handles[track.name]
            if head_count:
                positions = np.arange(start, start + head_count, dtype=np.int64)
                head = _read_mono(handle, start, head_count)
                tail = _read_mono(handle, self.manifest.frames + start, head_count)
                theta = (np.pi / 2.0) * ((positions.astype(np.float64) + 0.5) / crossfade_frames)
                signal = head.astype(np.float64) * np.sin(theta) + tail.astype(np.float64) * np.cos(theta)
                if apply_duck and track.scene_name == self.plan.reference_scene:
                    signal *= duck_gain[:head_count]
                left_gain, right_gain = _pan_for_frames(self.manifest, track, positions)
                output[:head_count, 0] += signal * track.mix_weight * left_gain
                output[:head_count, 1] += signal * track.mix_weight * right_gain

            remaining_start = start + head_count
            if remaining_start < end:
                count = end - remaining_start
                positions = np.arange(remaining_start, end, dtype=np.int64)
                signal = _read_mono(handle, remaining_start, count)
                if apply_duck and track.scene_name == self.plan.reference_scene:
                    signal = signal * duck_gain[head_count : head_count + count]
                left_gain, right_gain = _pan_for_frames(self.manifest, track, positions)
                output[head_count:, 0] += signal * track.mix_weight * left_gain
                output[head_count:, 1] += signal * track.mix_weight * right_gain

        return output.astype(np.float32)

    def _fold_mono(self, stereo: np.ndarray) -> np.ndarray:
        left_weight, right_weight = self.manifest.mono.fold_weights
        return (
            stereo[:, 0].astype(np.float64) * left_weight
            + stereo[:, 1].astype(np.float64) * right_weight
        ).astype(np.float32)

    def _render_block(self, index: int) -> tuple[np.ndarray, np.ndarray]:
        start = index * self.block_frames
        end = min(self.manifest.frames, start + self.block_frames)
        stereo = self._render_mix_block(start, end)
        return stereo, self._fold_mono(stereo)

    def _write_recovery_report(self, stop_after_blocks: bool = False) -> Path:
        payload = {
            "code": "RECOVERY_CHECKPOINT_COMMITTED",
            "status": "INTERRUPTED",
            "completed_blocks": self.completed_blocks,
            "total_blocks": self.total_blocks,
            "next_block": self.completed_blocks,
            "manifest_sha256": self.manifest.manifest_sha256,
            "decoupling_digest": self.plan_digest,
            "snapshot": str(self.snapshot_path.relative_to(self.output_dir)),
            "resume_command": "rainforest-master render --manifest <manifest> --out-dir <out-dir>",
            "forced_stop_after_block": bool(stop_after_blocks),
        }
        path = self.output_dir / RECOVERY_NAME
        return write_json_atomic(path, payload)

    def run(
        self,
        stop_requested: Callable[[], bool] | None = None,
        *,
        stop_after_blocks: int | None = None,
    ) -> RenderOutcome:
        stop_requested = stop_requested or (lambda: False)
        self.load_or_initialize()
        stop_signal = False
        while self.completed_blocks < self.total_blocks:
            if stop_requested():
                stop_signal = True
                break
            index = self.completed_blocks
            stereo, mono = self._render_block(index)
            self._commit_block(index, stereo, mono)
            if stop_after_blocks is not None and self.completed_blocks >= stop_after_blocks:
                stop_signal = True
                break
            if stop_requested():
                stop_signal = True
                break
        if stop_signal or stop_requested():
            self._write_recovery_report(stop_after_blocks is not None)
            return RenderOutcome(
                status="INTERRUPTED",
                completed_blocks=self.completed_blocks,
                total_blocks=self.total_blocks,
                interrupted=True,
            )
        report = self._finalize()
        return RenderOutcome(
            status="COMPLETE",
            completed_blocks=self.completed_blocks,
            total_blocks=self.total_blocks,
            report=report,
            report_path=self.output_dir / REPORT_NAME,
        )

    def _load_spool(self) -> tuple[np.ndarray, np.ndarray]:
        stereo = np.empty((self.manifest.frames, 2), dtype=np.float32)
        mono = np.empty((self.manifest.frames,), dtype=np.float32)
        for item in self.committed_blocks:
            start = int(item["start"])
            frames = int(item["frames"])
            stereo_path = self.state_dir / item["stereo_file"]
            mono_path = self.state_dir / item["mono_file"]
            stereo[start : start + frames] = np.fromfile(stereo_path, dtype="<f4").reshape(frames, 2)
            mono[start : start + frames] = np.fromfile(mono_path, dtype="<f4").reshape(frames)
        return stereo, mono

    def _master_audio(
        self, raw: np.ndarray
    ) -> tuple[np.ndarray, dict[str, Any], LimiterStats]:
        target = self.manifest.loudness.target_lufs
        tolerance = self.manifest.loudness.max_abs_deviation_lu
        limit = self.manifest.loudness.true_peak_limit_dbtp
        source_lufs = integrated_loudness_lufs(raw, self.manifest.sample_rate)
        gain = 10.0 ** ((target - source_lufs) / 20.0)
        seal_frames = min(
            int(round(self.manifest.loop.endpoint_seal_seconds * self.manifest.sample_rate)),
            self.manifest.crossfade_frames,
            self.manifest.frames // 2,
        )
        best: tuple[np.ndarray, dict[str, Any], LimiterStats] | None = None
        for _ in range(6):
            normalized = (raw.astype(np.float64) * gain).astype(np.float32)
            limiter_before = true_peak_dbtp(normalized, self.manifest.sample_rate)
            if limiter_before > limit:
                limited, limiter_stats = apply_peak_limiter(
                    normalized, limit, self.manifest.sample_rate
                )
            else:
                limited = normalized
                limiter_stats = LimiterStats(
                    triggered_samples=0,
                    pre_true_peak_dbtp=limiter_before,
                    post_true_peak_dbtp=limiter_before,
                    delay_compensation_samples=0,
                    gain_reduction_db=0.0,
                )
            finalized = seal_loop_endpoints(limited, seal_frames)
            lufs = integrated_loudness_lufs(finalized, self.manifest.sample_rate)
            peak = true_peak_dbtp(finalized, self.manifest.sample_rate)
            info = {
                "normalization_gain_db": float(20.0 * math.log10(max(gain, EPS))),
                "loudness_before_mastering_lufs": float(source_lufs),
                "loudness_after_mastering_lufs": float(lufs),
                "true_peak_before_limiter_dbtp": float(limiter_before),
                "true_peak_after_mastering_dbtp": float(peak),
                "limiter_triggered_samples": int(limiter_stats.triggered_samples),
                "limiter_delay_compensation_samples": int(limiter_stats.delay_compensation_samples),
                "limiter_gain_reduction_db": float(limiter_stats.gain_reduction_db),
            }
            best = (finalized, info, limiter_stats)
            if abs(lufs - target) <= tolerance and peak <= limit + 1.0e-6:
                break
            if not math.isfinite(lufs):
                break
            gain *= 10.0 ** ((target - lufs) / 20.0)
        assert best is not None
        return best

    def _scene_midband_stats(self) -> dict[str, Any]:
        scene_names = [scene for scene in ("虫鸣", "鸟鸣") if scene in self.manifest.tracks_by_scene]
        handles = {scene: sf.SoundFile(str(self.manifest.track(scene).path)) for scene in scene_names}
        block = 16_384
        window = np.hanning(block)
        frequencies = np.fft.rfftfreq(block, d=1.0 / self.manifest.sample_rate)
        band = (frequencies >= self.manifest.mono.mid_band_hz[0]) & (
            frequencies <= self.manifest.mono.mid_band_hz[1]
        )
        stereo_power = np.zeros(frequencies.shape[0], dtype=np.float64)
        mono_power = np.zeros(frequencies.shape[0], dtype=np.float64)
        segments = 0
        try:
            for start in range(0, self.manifest.frames, block):
                end = min(self.manifest.frames, start + block)
                count = end - start
                if count < 32:
                    continue
                indices = np.arange(start, end, dtype=np.int64)
                left = np.zeros(count, dtype=np.float64)
                right = np.zeros(count, dtype=np.float64)
                for scene in scene_names:
                    track = self.manifest.track(scene)
                    handle = handles[scene]
                    handle.seek(start)
                    signal = handle.read(count, dtype="float32", always_2d=False)
                    left_gain, right_gain = _pan_for_frames(self.manifest, track, indices)
                    left += signal * track.mix_weight * left_gain
                    right += signal * track.mix_weight * right_gain
                fold_left, fold_right = self.manifest.mono.fold_weights
                folded = left * fold_left + right * fold_right
                local_window = np.zeros(block, dtype=np.float64)
                local_window[:count] = np.hanning(count)
                padded_left = np.zeros(block, dtype=np.float64)
                padded_right = np.zeros(block, dtype=np.float64)
                padded_mono = np.zeros(block, dtype=np.float64)
                padded_left[:count] = left * local_window[:count]
                padded_right[:count] = right * local_window[:count]
                padded_mono[:count] = folded * local_window[:count]
                stereo_power += (
                    np.abs(np.fft.rfft(padded_left, n=block)) ** 2
                    + np.abs(np.fft.rfft(padded_right, n=block)) ** 2
                ) / max(count, 1)
                mono_power += np.abs(np.fft.rfft(padded_mono, n=block)) ** 2 / max(count, 1)
                segments += 1
        finally:
            for handle in handles.values():
                handle.close()
        if segments == 0:
            return {
                "stereo_scene_midband_ratio": 0.0,
                "mono_scene_midband_ratio": 0.0,
                "deviation_db": 0.0,
            }
        stereo_total = float(np.sum(stereo_power[1:]))
        mono_total = float(np.sum(mono_power[1:]))
        stereo_ratio = float(np.sum(stereo_power[band])) / max(stereo_total, EPS)
        mono_ratio = float(np.sum(mono_power[band])) / max(mono_total, EPS)
        deviation = abs(
            10.0 * math.log10(max(stereo_ratio, EPS) / max(mono_ratio, EPS))
        )
        return {
            "stereo_scene_midband_ratio": stereo_ratio,
            "mono_scene_midband_ratio": mono_ratio,
            "deviation_db": float(deviation),
        }

    def _pan_report(self) -> dict[str, Any]:
        reports: dict[str, Any] = {}
        threshold = self.manifest.pan.stability_threshold_db
        sample_rate = self.manifest.sample_rate
        chunk = 1_048_576
        transient_scenes = {"鸟鸣", "雨滴"}
        for track in self.manifest.tracks:
            ild_sum = 0.0
            ild_count = 0
            max_change = 0.0
            previous_ild: float | None = None
            side_jump = False
            for start in range(0, self.manifest.frames, chunk):
                end = min(self.manifest.frames, start + chunk)
                indices = np.arange(start, end, dtype=np.int64)
                left_gain, right_gain = _pan_for_frames(self.manifest, track, indices)
                if float(np.min(left_gain)) <= EPS or float(np.min(right_gain)) <= EPS:
                    side_jump = True
                ild = 20.0 * np.log10(np.maximum(left_gain, EPS) / np.maximum(right_gain, EPS))
                ild_sum += float(np.sum(ild))
                ild_count += int(ild.size)
                if previous_ild is not None:
                    max_change = max(max_change, abs(float(ild[0]) - previous_ild))
                if ild.size > 1:
                    max_change = max(max_change, float(np.max(np.abs(np.diff(ild)))))
                previous_ild = float(ild[-1])
            transient_checks: list[dict[str, Any]] = []
            if track.scene_name in transient_scenes:
                for seconds in track.transient_reference_times:
                    sample = int(round(seconds * sample_rate))
                    theta = pan_theta(track, sample, self.manifest.frames, sample_rate)
                    ild = 20.0 * math.log10(math.cos(theta) / math.sin(theta))
                    left_gain, right_gain = _pan_for_frames(
                        self.manifest, track, np.array([sample], dtype=np.int64)
                    )
                    measured = 20.0 * math.log10(
                        max(float(left_gain[0]), EPS) / max(float(right_gain[0]), EPS)
                    )
                    transient_checks.append(
                        {
                            "reference_time_seconds": float(seconds),
                            "sample": sample,
                            "expected_ild_db": float(ild),
                            "measured_ild_db": float(measured),
                            "error_db": abs(float(ild - measured)),
                        }
                    )
            reports[track.scene_name] = {
                "channel": track.name,
                "average_interaural_difference_db": ild_sum / max(1, ild_count),
                "max_adjacent_interaural_change_db": float(max_change),
                "stability_threshold_db": float(threshold),
                "passes_stability": bool(max_change <= threshold + 1.0e-12),
                "full_side_jump_detected": bool(side_jump),
                "transient_reference_checks": transient_checks,
                "transient_reference_max_error_db": (
                    max((item["error_db"] for item in transient_checks), default=0.0)
                ),
            }
        return reports

    def _finalize(self) -> dict[str, Any]:
        raw, raw_mono_spool = self._load_spool()
        energy_window = max(
            1,
            int(round(self.manifest.loop.seam_energy_length_seconds * self.manifest.sample_rate)),
        )
        seam_probe_start = max(0, self.manifest.frames - energy_window)
        seam_probe_end = min(self.manifest.source_frames, self.manifest.frames + energy_window)
        uncrossfaded = self._render_mix_block(
            seam_probe_start, seam_probe_end, crossfade=False, apply_duck=True
        )
        split = self.manifest.frames - seam_probe_start
        pre_tail = uncrossfaded[:split]
        pre_head = uncrossfaded[split:]
        if pre_head.shape[0] == 0:
            pre_head = raw[:1]
        pre_delta = float(
            np.max(
                np.abs(
                    uncrossfaded[min(split, uncrossfaded.shape[0] - 1)].astype(np.float64)
                    - uncrossfaded[max(0, min(split - 1, uncrossfaded.shape[0] - 1))].astype(np.float64)
                )
            )
        )
        pre_tail_energy = float(np.mean(pre_tail.astype(np.float64) ** 2)) if pre_tail.size else 0.0
        pre_head_energy = float(np.mean(pre_head.astype(np.float64) ** 2)) if pre_head.size else 0.0

        master, loudness_info, _ = self._master_audio(raw)
        left_weight, right_weight = self.manifest.mono.fold_weights
        mono = (
            master[:, 0].astype(np.float64) * left_weight
            + master[:, 1].astype(np.float64) * right_weight
        ).astype(np.float32)

        stereo_path = self.output_dir / STEREO_NAME
        mono_path = self.output_dir / MONO_NAME
        for output_path, audio in ((stereo_path, master), (mono_path, mono)):
            write_float32_wav(output_path, audio, self.manifest.sample_rate)

        seam = seam_metrics(
            master,
            self.manifest.sample_rate,
            self.manifest.loop.seam_energy_length_seconds,
            pre_delta,
        )
        seam.update(
            {
                "pre_crossfade_tail_energy": pre_tail_energy,
                "pre_crossfade_head_energy": pre_head_energy,
                "crossfade_seconds": self.manifest.loop.crossfade_seconds,
                "crossfade_frames": self.manifest.crossfade_frames,
                "max_allowed_sample_delta": self.manifest.loop.max_seam_sample_delta,
                "passes_sample_delta": bool(
                    seam["seam_sample_delta"] <= self.manifest.loop.max_seam_sample_delta + 1.0e-12
                ),
            }
        )

        mono_stats = midband_energy_ratio(
            master, mono, self.manifest.sample_rate, self.manifest.mono.mid_band_hz
        )
        scene_stats = self._scene_midband_stats()
        pan_report = self._pan_report()
        target_lufs = self.manifest.loudness.target_lufs
        lufs_delta = abs(
            float(loudness_info["loudness_after_mastering_lufs"]) - target_lufs
        )
        true_peak_ok = (
            float(loudness_info["true_peak_after_mastering_dbtp"])
            <= self.manifest.loudness.true_peak_limit_dbtp + 1.0e-6
        )
        loudness_ok = lufs_delta <= self.manifest.loudness.max_abs_deviation_lu
        mono_ok = (
            scene_stats["deviation_db"]
            <= self.manifest.mono.energy_ratio_tolerance_db + 1.0e-12
        )
        masking_ok = all(
            item.get("passes_snr", False) for item in self.plan.channel_reports.values()
        )
        checks = {
            "sample_count_exact": bool(master.shape[0] == self.manifest.frames),
            "mono_sample_count_exact": bool(mono.shape[0] == self.manifest.frames),
            "loop_sample_delta": seam["passes_sample_delta"],
            "loop_energy_jump": seam["seam_energy_jump_ok"],
            "pan_stability": all(
                item["passes_stability"] and not item["full_side_jump_detected"]
                for item in pan_report.values()
            ),
            "masking_snr_improvement": masking_ok,
            "loudness_target": bool(loudness_ok),
            "true_peak_limit": bool(true_peak_ok),
            "mono_energy_ratio": bool(mono_ok),
        }
        report = {
            "schema_version": 1,
            "status": "PASS" if all(checks.values()) else "FAIL",
            "algorithm_version": ALGORITHM_VERSION,
            "manifest_sha256": self.manifest.manifest_sha256,
            "manifest_canonical_sha256": self.manifest.canonical_digest(),
            "seed": self.manifest.seed,
            "sample_rate": self.manifest.sample_rate,
            "duration_seconds": self.manifest.duration_seconds,
            "frames": self.manifest.frames,
            "source_frames": self.manifest.source_frames,
            "rendering": {
                "block_frames": self.block_frames,
                "total_blocks": self.total_blocks,
                "completed_blocks": self.completed_blocks,
            },
            "outputs": {
                "stereo": {
                    "file": STEREO_NAME,
                    "channels": 2,
                    "frames": int(master.shape[0]),
                    "subtype": "FLOAT",
                    "bit_depth": 32,
                    "sha256": _sha256_file(stereo_path),
                },
                "mono": {
                    "file": MONO_NAME,
                    "channels": 1,
                    "frames": int(mono.shape[0]),
                    "subtype": "FLOAT",
                    "bit_depth": 32,
                    "sha256": _sha256_file(mono_path),
                },
            },
            "loop": seam,
            "panning": {
                "model": "equal_power_slow_analytic_lfo",
                "stability_threshold_db": self.manifest.pan.stability_threshold_db,
                "channels": pan_report,
            },
            "masking": self.plan.as_dict(),
            "mastering": {
                **loudness_info,
                "target_lufs": target_lufs,
                "loudness_delta_lu": float(lufs_delta),
                "max_abs_deviation_lu": self.manifest.loudness.max_abs_deviation_lu,
                "true_peak_limit_dbtp": self.manifest.loudness.true_peak_limit_dbtp,
                "limiter_type": "stereo_linked_zero_net_delay",
            },
            "mono_compatibility": {
                "fold_weights": list(self.manifest.mono.fold_weights),
                "mid_band_hz": list(self.manifest.mono.mid_band_hz),
                "master_ratio": mono_stats,
                "bird_insect_scene_ratio": scene_stats,
                "tolerance_db": self.manifest.mono.energy_ratio_tolerance_db,
                "passes": bool(mono_ok),
            },
            "checks": checks,
        }
        report_path = self.output_dir / REPORT_NAME
        write_json_atomic(report_path, report)
        return report

