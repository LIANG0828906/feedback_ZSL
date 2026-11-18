from __future__ import annotations

import hashlib
import math
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .dsp import (
    DuckResult,
    FitResult,
    compute_ducking,
    fit_to_length,
    lookahead_limit,
    true_peak_dbtp,
)
from .errors import MasteringError, MasteringIssue, issue
from .loudness import active_ratio, integrated_loudness
from .project import Project, TrackSpec, load_project
from .state import StateStore, canonical_json_bytes, stable_json_bytes
from .wavio import (
    WavInfo,
    atomic_copy,
    atomic_write_bytes,
    db_to_gain,
    encode_pcm,
    probe_wav,
    quantize_pcm,
    read_wav,
    sha256_file,
    wav_header,
)


@dataclass(frozen=True)
class LoadedTrack:
    spec: TrackSpec
    samples: np.ndarray
    info: WavInfo


@dataclass
class RuntimeLanguage:
    language: str
    voice_track: TrackSpec
    fit: FitResult
    voice_gain_db: float
    loudness_before_lufs: float | None
    loudness_after_gain_lufs: float | None
    active: float
    silent: bool
    music_gain_db: float
    sfx_gain_db: float
    program_gain_db: float
    ducking: DuckResult
    limiter: Any
    prepared: np.ndarray
    master_loudness_lufs: float | None
    master_true_peak_dbtp: float
    warnings: list[dict[str, str]]


@dataclass
class RunResult:
    status: str
    exit_code: int
    payload: dict[str, Any]


class InterruptController:
    def __init__(
        self,
        interrupt_after_blocks: int | None = None,
        crash_after_blocks: int | None = None,
    ):
        self.interrupted = False
        self.interrupt_after_blocks = interrupt_after_blocks
        self.crash_after_blocks = crash_after_blocks

    def request_interrupt(self, signum: int | None = None, frame: Any = None) -> None:
        del signum, frame
        self.interrupted = True

    def graceful_stop_after(self, completed_blocks: int) -> bool:
        if self.interrupted:
            return True
        return (
            self.interrupt_after_blocks is not None
            and completed_blocks >= self.interrupt_after_blocks
        )

    def crash_after(self, completed_blocks: int) -> bool:
        return (
            self.crash_after_blocks is not None
            and completed_blocks >= self.crash_after_blocks
        )


class MasteringEngine:
    def __init__(
        self,
        project_path: Path | str,
        *,
        rules_path: Path | str | None = None,
        output_dir: Path | str | None = None,
        state_dir: Path | str | None = None,
        block_frames: int | None = None,
        controller: InterruptController | None = None,
        progress_callback: Callable[[int, int], None] | None = None,
    ):
        self.project_path = Path(project_path)
        self.rules_path = Path(rules_path) if rules_path is not None else None
        self.output_override = Path(output_dir) if output_dir is not None else None
        self.state_override = Path(state_dir) if state_dir is not None else None
        self.block_frames_override = block_frames
        self.controller = controller or InterruptController()
        self.progress_callback = progress_callback

    def run(self) -> RunResult:
        project, issues, _canonical = load_project(
            self.project_path,
            rules_override=self.rules_path,
            output_override=self.output_override,
            block_frames_override=self.block_frames_override,
        )
        if project is None:
            raise MasteringError(issues)

        tracks, issues = self._load_tracks(project)
        if issues:
            raise MasteringError(issues)
        fitted, issues = self._fit_tracks(project, tracks)
        if issues:
            raise MasteringError(issues)

        project_digest = self._project_digest(project, tracks)
        total_frames = fitted["music"].samples.shape[0]
        language_order = self._language_order(project)
        state = StateStore(project.output_dir, self.state_override)
        loaded = state.load(project_digest, project.block_frames, total_frames, language_order)
        if loaded.snapshot is not None and loaded.resumed:
            state.prepare_resume(loaded.snapshot, language_order)
        project.output_dir.mkdir(parents=True, exist_ok=True)
        state.work_dir.mkdir(parents=True, exist_ok=True)

        if loaded.snapshot is None:
            runtimes = self._analyze_fresh(project, fitted, language_order)
            analysis = self._analysis_snapshot(project, runtimes, fitted)
            stage_files = self._create_stage_files(
                state, project, language_order, total_frames
            )
            snapshot = state.initial_snapshot(
                project_digest,
                project.block_frames,
                total_frames,
                analysis,
                stage_files,
                list(loaded.discarded_versions),
            )
            snapshot = state.commit(snapshot)
        else:
            snapshot = dict(loaded.snapshot)
            snapshot["version"] = int(snapshot["version"]) + 1
            snapshot["recovery_count"] = int(snapshot["recovery_count"]) + 1
            snapshot = state.commit(snapshot)
            runtimes = self._analyze_resumed(project, fitted, language_order, snapshot)

        prepared = {
            runtime.language: runtime.prepared
            for runtime in runtimes
        }
        interrupted = self._write_blocks(
            state,
            snapshot,
            project,
            language_order,
            prepared,
        )
        if interrupted:
            payload = play_state_payload(
                "interrupted",
                snapshot,
            )
            state.write_recovery_report(payload)
            return RunResult(status="interrupted", exit_code=0, payload=payload)

        report = self._finalize(
            state,
            snapshot,
            project,
            language_order,
            runtimes,
        )
        completion_state = play_state_payload("completed", snapshot)
        completion_state["outputs"] = [
            entry["file"]["path"] for entry in report["languages"]
        ]
        state.write_recovery_report(completion_state)
        state.cleanup()
        return RunResult(status="completed", exit_code=0, payload=report)

    def _load_tracks(
        self, project: Project
    ) -> tuple[dict[str, LoadedTrack], list[MasteringIssue]]:
        loaded: dict[str, LoadedTrack] = {}
        issues: list[MasteringIssue] = []
        for spec in project.tracks:
            samples, info, track_issues = read_wav(spec.path, spec.name)
            issues.extend(track_issues)
            if info is None:
                continue
            expected_rate = project.sample_rate
            expected_channels = project.channels
            expected_bits = project.bit_depth
            if info.sample_rate != expected_rate:
                issues.append(
                    issue(
                        "SAMPLE_RATE_MISMATCH",
                        spec.name,
                        f"WAV declares {info.sample_rate} Hz; project requires {expected_rate} Hz",
                    )
                )
            if info.channels != expected_channels:
                issues.append(
                    issue(
                        "CHANNEL_COUNT_MISMATCH",
                        spec.name,
                        f"WAV declares {info.channels} channels; project requires {expected_channels}",
                    )
                )
            if info.bit_depth != expected_bits:
                issues.append(
                    issue(
                        "BIT_DEPTH_MISMATCH",
                        spec.name,
                        f"WAV declares {info.bit_depth}-bit; project requires {expected_bits}-bit",
                    )
                )
            if samples is None:
                continue
            if info.frames == 0:
                issues.append(issue("EMPTY_TRACK", spec.name, "track contains no audio frames"))
                continue
            loaded[spec.name] = LoadedTrack(spec=spec, samples=samples, info=info)
        return loaded, issues

    def _fit_tracks(
        self,
        project: Project,
        loaded: dict[str, LoadedTrack],
    ) -> tuple[dict[str, LoadedTrack | FitResult], list[MasteringIssue]]:
        issues: list[MasteringIssue] = []
        music_tracks = [spec for spec in project.tracks if spec.role == "music"]
        if len(music_tracks) != 1 or music_tracks[0].name not in loaded:
            return {}, issues
        music_track = loaded[music_tracks[0].name]
        total_frames = music_track.samples.shape[0]
        fitted: dict[str, LoadedTrack | FitResult] = {"music": music_track}

        for spec in project.tracks:
            if spec.role in ("music",):
                continue
            if spec.name not in loaded:
                continue
            fit, error = fit_to_length(
                loaded[spec.name].samples,
                total_frames,
                project.sample_rate,
                project.silence.threshold_dbfs,
            )
            if fit is None:
                issues.append(issue("TRIM_WOULD_REMOVE_AUDIO", spec.name, error or "cannot fit track"))
                continue
            fitted[spec.name] = fit
        return fitted, issues

    def _language_order(self, project: Project) -> list[str]:
        voices = {track.language: track for track in project.tracks if track.role == "voice"}
        ordered = [language for language in project.required_languages if language in voices]
        ordered.extend(sorted(language for language in voices if language not in ordered))
        return ordered

    def _project_digest(
        self,
        project: Project,
        loaded: dict[str, LoadedTrack],
    ) -> str:
        file_hashes = {
            spec.name: sha256_file(spec.path)
            for spec in project.tracks
            if spec.name in loaded
        }
        payload = {
            "schema": "uid211.project-digest.v1",
            "canonical": project.canonical,
            "file_hashes": file_hashes,
            "block_frames": project.block_frames,
        }
        return hashlib.sha256(canonical_json_bytes(payload)).hexdigest()

    def _bus_gain(
        self,
        samples: np.ndarray,
        sample_rate: int,
        target_lufs: float | None,
        base_gain_db: float,
    ) -> tuple[float, float | None]:
        measured = integrated_loudness(samples, sample_rate)
        if target_lufs is None or measured is None:
            return base_gain_db, measured
        return base_gain_db + target_lufs - measured, measured

    def _analysis_snapshot(
        self,
        project: Project,
        runtimes: list[RuntimeLanguage],
        fitted: dict[str, LoadedTrack | FitResult],
    ) -> dict[str, Any]:
        music_track = fitted["music"]
        assert isinstance(music_track, LoadedTrack)
        languages: dict[str, Any] = {}
        for runtime in runtimes:
            languages[runtime.language] = {
                "track": runtime.voice_track.name,
                "voice_gain_db": float(runtime.voice_gain_db),
                "voice_loudness_before_lufs": runtime.loudness_before_lufs,
                "voice_loudness_after_gain_lufs": runtime.loudness_after_gain_lufs,
                "voice_active_ratio": float(runtime.active),
                "silent": bool(runtime.silent),
                "warnings": runtime.warnings,
                "music_gain_db": float(runtime.music_gain_db),
                "sfx_gain_db": float(runtime.sfx_gain_db),
                "program_gain_db": float(runtime.program_gain_db),
                "ducking": {
                    "depth_db": float(project.ducking.depth_db),
                    "threshold_dbfs": float(project.ducking.threshold_dbfs),
                    "attack_ms": float(project.ducking.attack_ms),
                    "release_ms": float(project.ducking.release_ms),
                    "frame_ms": float(project.ducking.frame_ms),
                    "ducked_samples": int(runtime.ducking.ducked_samples),
                    "max_attenuation_db": float(runtime.ducking.max_attenuation_db),
                },
                "limiter": self._limiter_snapshot(runtime.limiter),
                "master_loudness_lufs": runtime.master_loudness_lufs,
                "master_true_peak_dbtp": float(runtime.master_true_peak_dbtp),
            }
        return {
            "tracks": {
                "music": music_track.spec.name,
                "sfx": next(
                    track.name for track in project.tracks if track.role == "sfx"
                ),
            },
            "languages": languages,
        }

    def _limiter_snapshot(self, limiter: Any) -> dict[str, Any]:
        return {
            "triggered_samples": int(limiter.triggered_samples),
            "max_attenuation_db": float(limiter.max_attenuation_db),
            "lookahead_samples": int(limiter.lookahead_samples),
            "delay_compensation_samples": int(limiter.delay_compensation_samples),
            "measured_true_peak_dbtp": float(limiter.measured_true_peak_dbtp),
        }

    def _analyze_fresh(
        self,
        project: Project,
        fitted: dict[str, LoadedTrack | FitResult],
        language_order: list[str],
    ) -> list[RuntimeLanguage]:
        music_loaded = fitted["music"]
        sfx_fit = fitted["sfx"]
        assert isinstance(music_loaded, LoadedTrack)
        assert isinstance(sfx_fit, FitResult)
        music_gain, music_measured = self._bus_gain(
            music_loaded.samples,
            project.sample_rate,
            project.music_target_lufs,
            project.music_gain_db,
        )
        sfx_gain, sfx_measured = self._bus_gain(
            sfx_fit.samples,
            project.sample_rate,
            project.sfx_target_lufs,
            project.sfx_gain_db,
        )
        bus = music_loaded.samples * db_to_gain(music_gain)
        bus += sfx_fit.samples * db_to_gain(sfx_gain)

        runtimes: list[RuntimeLanguage] = []
        for language in language_order:
            voice_spec = next(
                track
                for track in project.tracks
                if track.role == "voice" and track.language == language
            )
            fit = fitted[voice_spec.name]
            assert isinstance(fit, FitResult)
            rule = project.languages[language]
            loudness_before = integrated_loudness(fit.samples, project.sample_rate)
            activity = active_ratio(
                fit.samples,
                project.sample_rate,
                project.silence.threshold_dbfs,
                project.silence.frame_ms,
            )
            silent = loudness_before is None or activity < project.silence.min_active_ratio
            warnings: list[dict[str, str]] = []
            if silent:
                voice_gain = 0.0
                loudness_after = loudness_before
                warnings.append(
                    {
                        "code": "SILENT_TRACK",
                        "track": voice_spec.name,
                        "message": (
                            "active ratio is below the configured threshold; "
                            "gain adjustment was skipped to avoid amplifying noise"
                        ),
                    }
                )
            else:
                voice_gain = rule.target_lufs - float(loudness_before)
                gained = fit.samples * db_to_gain(voice_gain)
                loudness_after = integrated_loudness(gained, project.sample_rate)
            gained_voice = fit.samples * db_to_gain(voice_gain)
            duck = compute_ducking(
                gained_voice,
                project.sample_rate,
                project.ducking.depth_db,
                project.ducking.threshold_dbfs,
                project.ducking.attack_ms,
                project.ducking.release_ms,
                project.ducking.frame_ms,
            )
            base_mix = gained_voice + bus * duck.gain[:, None]
            runtime = self._render_runtime(
                project,
                language,
                voice_spec,
                fit,
                voice_gain,
                loudness_before,
                loudness_after,
                activity,
                silent,
                music_gain,
                sfx_gain,
                base_mix,
                duck,
                warnings,
            )
            runtimes.append(runtime)
        return runtimes

    def _analyze_resumed(
        self,
        project: Project,
        fitted: dict[str, LoadedTrack | FitResult],
        language_order: list[str],
        snapshot: dict[str, Any],
    ) -> list[RuntimeLanguage]:
        analysis = snapshot["analysis"]["languages"]
        music_loaded = fitted["music"]
        sfx_fit = fitted["sfx"]
        assert isinstance(music_loaded, LoadedTrack)
        assert isinstance(sfx_fit, FitResult)
        runtimes: list[RuntimeLanguage] = []
        for language in language_order:
            params = analysis[language]
            voice_spec = next(
                track
                for track in project.tracks
                if track.role == "voice" and track.language == language
            )
            fit = fitted[voice_spec.name]
            assert isinstance(fit, FitResult)
            voice_gain = float(params["voice_gain_db"])
            music_gain = float(params["music_gain_db"])
            sfx_gain = float(params["sfx_gain_db"])
            gained_voice = fit.samples * db_to_gain(voice_gain)
            duck_params = params["ducking"]
            duck = compute_ducking(
                gained_voice,
                project.sample_rate,
                float(duck_params["depth_db"]),
                float(duck_params["threshold_dbfs"]),
                float(duck_params["attack_ms"]),
                float(duck_params["release_ms"]),
                float(duck_params["frame_ms"]),
            )
            bus = music_loaded.samples * db_to_gain(music_gain)
            bus += sfx_fit.samples * db_to_gain(sfx_gain)
            base_mix = gained_voice + bus * duck.gain[:, None]
            runtime = self._render_runtime(
                project,
                language,
                voice_spec,
                fit,
                voice_gain,
                params.get("voice_loudness_before_lufs"),
                params.get("voice_loudness_after_gain_lufs"),
                float(params.get("voice_active_ratio", 0.0)),
                bool(params.get("silent", False)),
                music_gain,
                sfx_gain,
                base_mix,
                duck,
                list(params.get("warnings", [])),
                program_gain_override=float(params["program_gain_db"]),
            )
            runtimes.append(runtime)
        return runtimes

    def _render_runtime(
        self,
        project: Project,
        language: str,
        voice_spec: TrackSpec,
        fit: FitResult,
        voice_gain: float,
        loudness_before: float | None,
        loudness_after: float | None,
        activity: float,
        silent: bool,
        music_gain: float,
        sfx_gain: float,
        base_mix: np.ndarray,
        duck: DuckResult,
        warnings: list[dict[str, str]],
        *,
        program_gain_override: float | None = None,
    ) -> RuntimeLanguage:
        rule = project.languages[language]
        limiter_rule = project.limiter
        if program_gain_override is None:
            program_gain = 0.0
            for _ in range(14):
                candidate = lookahead_limit(
                    base_mix * db_to_gain(program_gain),
                    project.sample_rate,
                    rule.true_peak_dbtp,
                    limiter_rule.lookahead_ms,
                    limiter_rule.release_ms,
                    limiter_rule.oversample,
                    limiter_rule.safety_margin_db,
                )
                measured = integrated_loudness(candidate.samples, project.sample_rate)
                if measured is None:
                    break
                error = rule.target_lufs - measured
                if abs(error) <= 0.03:
                    break
                program_gain += max(-6.0, min(6.0, error))
                program_gain = max(-40.0, min(40.0, program_gain))
        else:
            program_gain = program_gain_override

        candidate = None
        quantized_metrics: tuple[float | None, float] | None = None
        for _ in range(5):
            candidate = lookahead_limit(
                base_mix * db_to_gain(program_gain),
                project.sample_rate,
                rule.true_peak_dbtp,
                limiter_rule.lookahead_ms,
                limiter_rule.release_ms,
                limiter_rule.oversample,
                limiter_rule.safety_margin_db,
            )
            quantized_float = (
                quantize_pcm(candidate.samples, project.bit_depth).astype(np.float64)
                / float(1 << (project.bit_depth - 1))
            )
            q_loudness = integrated_loudness(quantized_float, project.sample_rate)
            q_peak = true_peak_dbtp(quantized_float, limiter_rule.oversample)
            quantized_metrics = (q_loudness, q_peak)
            if q_peak <= rule.true_peak_dbtp + 1e-9 and (
                q_loudness is None or abs(q_loudness - rule.target_lufs) <= 0.1
            ):
                break
            if q_peak > rule.true_peak_dbtp:
                program_gain -= (q_peak - rule.true_peak_dbtp) + 0.01
            elif q_loudness is not None and q_loudness < rule.target_lufs - 0.1:
                program_gain += min(0.2, rule.target_lufs - q_loudness)

        assert candidate is not None and quantized_metrics is not None
        q_loudness, q_peak = quantized_metrics
        if q_peak > rule.true_peak_dbtp + 1e-6:
            raise MasteringError(
                [
                    issue(
                        "TRUE_PEAK_UNREACHABLE",
                        voice_spec.name,
                        f"could not place master below {rule.true_peak_dbtp} dBTP",
                    )
                ],
                exit_code=3,
            )
        if q_loudness is not None and abs(q_loudness - rule.target_lufs) > 0.5:
            raise MasteringError(
                [
                    issue(
                        "LOUDNESS_TARGET_UNREACHABLE",
                        voice_spec.name,
                        f"master loudness {q_loudness:.3f} LUFS is outside "
                        f"target {rule.target_lufs:.3f} +/- 0.5 LU",
                    )
                ],
                exit_code=3,
            )
        return RuntimeLanguage(
            language=language,
            voice_track=voice_spec,
            fit=fit,
            voice_gain_db=float(voice_gain),
            loudness_before_lufs=loudness_before,
            loudness_after_gain_lufs=loudness_after,
            active=float(activity),
            silent=bool(silent),
            music_gain_db=float(music_gain),
            sfx_gain_db=float(sfx_gain),
            program_gain_db=float(program_gain),
            ducking=duck,
            limiter=candidate,
            prepared=candidate.samples,
            master_loudness_lufs=q_loudness,
            master_true_peak_dbtp=float(q_peak),
            warnings=warnings,
        )

    def _create_stage_files(
        self,
        state: StateStore,
        project: Project,
        languages: list[str],
        total_frames: int,
    ) -> dict[str, dict[str, Any]]:
        block_align = project.channels * ((project.bit_depth + 7) // 8)
        total_bytes = 44 + total_frames * block_align
        entries: dict[str, dict[str, Any]] = {}
        for language in languages:
            relative = f"work/master_{language}.wav"
            path = state.stage_path(relative)
            path.parent.mkdir(parents=True, exist_ok=True)
            header = wav_header(
                project.sample_rate,
                project.channels,
                project.bit_depth,
                total_frames,
            )
            with path.open("wb") as handle:
                handle.write(header)
                handle.flush()
                os.fsync(handle.fileno())
            entries[language] = {
                "name": relative,
                "committed_bytes": 44,
                "total_bytes": total_bytes,
                "header_sha256": hashlib.sha256(header).hexdigest(),
            }
        return entries

    def _write_blocks(
        self,
        state: StateStore,
        snapshot: dict[str, Any],
        project: Project,
        languages: list[str],
        prepared: dict[str, np.ndarray],
    ) -> bool:
        total_frames = int(snapshot["total_frames"])
        block_frames = int(snapshot["block_frames"])
        total_blocks = int(snapshot["total_blocks"])
        completed = int(snapshot["completed_blocks"])
        block_align = project.channels * ((project.bit_depth + 7) // 8)
        handles = {
            language: state.stage_path(snapshot["stage_files"][language]["name"]).open("r+b")
            for language in languages
        }
        try:
            for block_index in range(completed, total_blocks):
                start = block_index * block_frames
                end = min(total_frames, start + block_frames)
                for language in languages:
                    chunk = prepared[language][start:end]
                    payload = encode_pcm(chunk, project.bit_depth)
                    handle = handles[language]
                    handle.seek(44 + start * block_align)
                    handle.write(payload)
                    handle.flush()
                    os.fsync(handle.fileno())
                completed_now = block_index + 1
                snapshot["version"] = int(snapshot["version"]) + 1
                snapshot["completed_blocks"] = completed_now
                for language in languages:
                    snapshot["stage_files"][language]["committed_bytes"] = (
                        44 + end * block_align
                    )
                state.commit(snapshot)
                if self.progress_callback is not None:
                    self.progress_callback(completed_now, total_blocks)
                if self.controller.crash_after(completed_now):
                    os._exit(137)
                if self.controller.graceful_stop_after(completed_now):
                    return completed_now < total_blocks
        finally:
            for handle in handles.values():
                handle.close()
        return False

    def _finalize(
        self,
        state: StateStore,
        snapshot: dict[str, Any],
        project: Project,
        languages: list[str],
        runtimes: list[RuntimeLanguage],
    ) -> dict[str, Any]:
        master_metrics: dict[str, Any] = {}
        file_entries: dict[str, dict[str, Any]] = {}
        language_warnings: list[dict[str, str]] = []
        for runtime in runtimes:
            language = runtime.language
            stage_path = state.stage_path(snapshot["stage_files"][language]["name"])
            samples, info, read_issues = read_wav(stage_path, runtime.voice_track.name)
            if read_issues or samples is None or info is None:
                raise MasteringError(
                    [
                        issue(
                            "OUTPUT_WRITE_FAILED",
                            runtime.voice_track.name,
                            "finalized stage WAV could not be read back",
                        )
                    ],
                    exit_code=3,
                )
            if info.frames != snapshot["total_frames"]:
                raise MasteringError(
                    [
                        issue(
                            "OUTPUT_WRITE_FAILED",
                            runtime.voice_track.name,
                            "finalized WAV frame count is inconsistent",
                        )
                    ],
                    exit_code=3,
                )
            loudness = integrated_loudness(samples, project.sample_rate)
            peak = true_peak_dbtp(samples, project.limiter.oversample)
            rule = project.languages[language]
            if peak > rule.true_peak_dbtp + 1e-6:
                raise MasteringError(
                    [
                        issue(
                            "TRUE_PEAK_EXCEEDED",
                            runtime.voice_track.name,
                            f"final WAV true peak {peak:.6f} dBTP exceeds {rule.true_peak_dbtp}",
                        )
                    ],
                    exit_code=3,
                )
            if loudness is None or abs(loudness - rule.target_lufs) > 0.5:
                raise MasteringError(
                    [
                        issue(
                            "LOUDNESS_TARGET_UNREACHABLE",
                            runtime.voice_track.name,
                            "final WAV loudness is outside the required target tolerance",
                        )
                    ],
                    exit_code=3,
                )
            output_name = f"master_{language}.wav"
            final_path = project.output_dir / output_name
            atomic_copy(stage_path, final_path)
            payload_hash = sha256_file(final_path)
            master_metrics[language] = {
                "loudness_lufs": loudness,
                "true_peak_dbtp": peak,
            }
            file_entries[language] = {
                "path": output_name,
                "sha256": payload_hash,
                "bytes": final_path.stat().st_size,
            }
            language_warnings.extend(runtime.warnings)
        report = self._build_report(
            project,
            snapshot,
            runtimes,
            master_metrics,
            file_entries,
            language_warnings,
        )
        atomic_write_bytes(
            project.output_dir / "mix_report.json",
            stable_json_bytes(report),
        )
        return report

    def _build_report(
        self,
        project: Project,
        snapshot: dict[str, Any],
        runtimes: list[RuntimeLanguage],
        master_metrics: dict[str, Any],
        file_entries: dict[str, dict[str, Any]],
        warnings: list[dict[str, str]],
    ) -> dict[str, Any]:
        by_language = {runtime.language: runtime for runtime in runtimes}
        language_reports: list[dict[str, Any]] = []
        for language in [runtime.language for runtime in runtimes]:
            runtime = by_language[language]
            rule = project.languages[language]
            metrics = master_metrics[language]
            language_reports.append(
                {
                    "language": language,
                    "voice_track": runtime.voice_track.name,
                    "voice": {
                        "original_frames": runtime.fit.original_frames,
                        "original_duration_seconds": _number(
                            runtime.fit.original_frames / project.sample_rate
                        ),
                        "adapted_frames": runtime.fit.adapted_frames,
                        "adapted_duration_seconds": _number(
                            runtime.fit.adapted_frames / project.sample_rate
                        ),
                        "trimmed_samples": runtime.fit.trimmed_samples,
                        "padded_samples": runtime.fit.padded_samples,
                        "active_ratio": _number(runtime.active),
                        "silent": runtime.silent,
                        "loudness_before_lufs": _number(runtime.loudness_before_lufs),
                        "target_lufs": _number(rule.target_lufs),
                        "gain_db": _number(runtime.voice_gain_db),
                        "loudness_after_gain_lufs": _number(
                            runtime.loudness_after_gain_lufs
                        ),
                        "warnings": runtime.warnings,
                    },
                    "ducking": {
                        "bus": "music+sfx",
                        "depth_db": _number(project.ducking.depth_db),
                        "threshold_dbfs": _number(project.ducking.threshold_dbfs),
                        "attack_ms": _number(project.ducking.attack_ms),
                        "release_ms": _number(project.ducking.release_ms),
                        "ducked_samples": runtime.ducking.ducked_samples,
                        "ducked_seconds": _number(
                            runtime.ducking.ducked_samples / project.sample_rate
                        ),
                        "music_bus_ducked_samples": runtime.ducking.ducked_samples,
                        "music_bus_ducked_seconds": _number(
                            runtime.ducking.ducked_samples / project.sample_rate
                        ),
                        "max_attenuation_db": _number(
                            runtime.ducking.max_attenuation_db
                        ),
                    },
                    "master": {
                        "total_frames": snapshot["total_frames"],
                        "total_seconds": _number(
                            snapshot["total_frames"] / project.sample_rate
                        ),
                        "loudness_lufs": _number(metrics["loudness_lufs"]),
                        "target_lufs": _number(rule.target_lufs),
                        "true_peak_dbtp": _number(metrics["true_peak_dbtp"]),
                        "true_peak_limit_dbtp": _number(rule.true_peak_dbtp),
                        "true_peak_oversample": int(project.limiter.oversample),
                        "limiter_triggered_samples": int(
                            runtime.limiter.triggered_samples
                        ),
                        "limiter_max_attenuation_db": _number(
                            runtime.limiter.max_attenuation_db
                        ),
                        "lookahead_samples": int(runtime.limiter.lookahead_samples),
                        "delay_compensation_samples": int(
                            runtime.limiter.delay_compensation_samples
                        ),
                    },
                    "file": file_entries[language],
                }
            )
        return {
            "schema": "uid211.mix-report.v1",
            "project_digest": snapshot["project_digest"],
            "sample_rate": project.sample_rate,
            "channels": project.channels,
            "bit_depth": project.bit_depth,
            "total_frames": snapshot["total_frames"],
            "total_seconds": _number(snapshot["total_frames"] / project.sample_rate),
            "buses": {
                "music": {
                    "gain_db": _number(runtimes[0].music_gain_db),
                },
                "sfx": {
                    "gain_db": _number(runtimes[0].sfx_gain_db),
                },
            },
            "languages": language_reports,
            "warnings": warnings,
        }


def _number(value: float | None) -> float | None:
    if value is None:
        return None
    if not math.isfinite(float(value)):
        return None
    return round(float(value), 6)


def play_state_payload(status: str, snapshot: dict[str, Any]) -> dict[str, Any]:
    return {
        "status": status,
        "recovery_count": int(snapshot["recovery_count"]),
        "discarded_snapshot_versions": list(snapshot["discarded_snapshot_versions"]),
        "completed_blocks": int(snapshot["completed_blocks"]),
        "total_blocks": int(snapshot["total_blocks"]),
        "total_frames": int(snapshot["total_frames"]),
        "outputs": [],
    }

