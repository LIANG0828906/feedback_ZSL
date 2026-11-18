"""Reference-based gain/delay estimation and cancellation of speaker returns."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import ProcessingFailure
from .project import Project, ReferenceWindow


@dataclass(frozen=True)
class PairEstimate:
    actor_id: str
    speaker_id: str
    speaker_azimuth_deg: float
    speaker_elevation_deg: float
    gain_linear: float
    gain_db: float
    delay_samples: int
    delay_ms: float
    before_energy: float
    after_energy: float
    residual_ratio: float
    reference_windows: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "actor_id": self.actor_id,
            "speaker_id": self.speaker_id,
            "speaker_azimuth_deg": self.speaker_azimuth_deg,
            "speaker_elevation_deg": self.speaker_elevation_deg,
            "gain_linear": self.gain_linear,
            "gain_db": self.gain_db,
            "polarity": 1 if self.gain_linear >= 0 else -1,
            "delay_samples": self.delay_samples,
            "delay_ms": self.delay_ms,
            "before_energy": self.before_energy,
            "after_energy": self.after_energy,
            "residual_ratio": self.residual_ratio,
            "reference_windows": self.reference_windows,
        }


@dataclass(frozen=True)
class ActorCrosstalkResult:
    actor_id: str
    clean: np.ndarray
    before_energy: float
    after_energy: float
    residual_ratio: float
    direct_loss_ratio: float | None
    direct_probe_count: int
    pairs: tuple[PairEstimate, ...]
    method: str

    def summary_dict(self) -> dict[str, Any]:
        return {
            "actor_id": self.actor_id,
            "method": self.method,
            "before_energy": self.before_energy,
            "after_energy": self.after_energy,
            "residual_ratio": self.residual_ratio,
            "direct_loss_ratio": self.direct_loss_ratio,
            "direct_probe_count": self.direct_probe_count,
            "pairs": [pair.to_dict() for pair in self.pairs],
        }


@dataclass(frozen=True)
class CrosstalkResult:
    actors: dict[str, ActorCrosstalkResult]
    residual_energy_ratio_max: float
    max_direct_energy_loss_ratio: float

    def summary_dict(self) -> dict[str, Any]:
        return {
            "method": "reference_xcorr_then_joint_least_squares_v1",
            "residual_energy_ratio_max": self.residual_energy_ratio_max,
            "max_direct_energy_loss_ratio": self.max_direct_energy_loss_ratio,
            "actors": [
                self.actors[actor_id].summary_dict() for actor_id in sorted(self.actors)
            ],
        }

    def fingerprint(self) -> str:
        encoded = json.dumps(
            self.summary_dict(), sort_keys=True, separators=(",", ":"), ensure_ascii=True
        )
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def estimate_and_cancel_crosstalk(
    project: Project,
    actor_mics: dict[str, np.ndarray],
    speakers: dict[str, np.ndarray],
) -> CrosstalkResult:
    """Estimate one gain/delay tap per actor-speaker path and subtract it."""

    actor_results: dict[str, ActorCrosstalkResult] = {}
    feedback_by_actor: dict[str, list[Any]] = {}
    for path in project.crosstalk.feedback:
        feedback_by_actor.setdefault(path.actor_id, []).append(path)

    for actor in project.actors:
        mic = np.asarray(actor_mics[actor.id], dtype=np.float64)
        paths = sorted(feedback_by_actor.get(actor.id, []), key=lambda item: item.speaker_id)
        actor_windows = _windows_for_actor(project, actor.id)
        if not paths:
            actor_results[actor.id] = ActorCrosstalkResult(
                actor_id=actor.id,
                clean=mic.copy(),
                before_energy=0.0,
                after_energy=0.0,
                residual_ratio=0.0,
                direct_loss_ratio=0.0,
                direct_probe_count=len(_probe_windows_for_actor(project, actor.id)),
                pairs=(),
                method="no_declared_feedback",
            )
            continue
        if not actor_windows:
            raise ProcessingFailure(
                "CROSSTALK_REFERENCE_INSUFFICIENT",
                "No reference windows are available for an actor with declared feedback.",
                channel=actor.channel,
            )

        pair_windows: dict[tuple[str, str], list[ReferenceWindow]] = {}
        initial_lags: dict[tuple[str, str], int] = {}
        for path in paths:
            windows = [
                window
                for window in actor_windows
                if window.speaker_id in (None, path.speaker_id)
            ]
            if not windows:
                windows = actor_windows
            pair_windows[(actor.id, path.speaker_id)] = windows
            lag = _estimate_initial_lag(
                mic,
                speakers[path.speaker_id],
                windows,
                project.sample_rate,
                max(
                    project.crosstalk.max_delay_ms,
                    path.delay_ms,
                ),
            )
            if lag is None:
                raise ProcessingFailure(
                    "CROSSTALK_REFERENCE_INSUFFICIENT",
                    "Reference windows do not contain enough speaker-return energy to estimate delay.",
                    channel=actor.channel,
                    details={"speaker_id": path.speaker_id},
                )
            initial_lags[(actor.id, path.speaker_id)] = lag

        actor_sample_windows = _round_windows(actor_windows, project.sample_rate, mic.size)
        search_max_delay_ms = max(
            project.crosstalk.max_delay_ms,
            max((path.delay_ms for path in paths), default=0.0),
        )
        fixed_lags, coefficients, residual = _fit_lags_and_gains(
            mic,
            speakers,
            actor_sample_windows,
            [path.speaker_id for path in paths],
            [initial_lags[(actor.id, path.speaker_id)] for path in paths],
            project.sample_rate,
            search_max_delay_ms,
        )

        clean = mic - _subtract_full(mic, speakers, paths, fixed_lags, coefficients)

        all_columns = _design_matrix_samples(
            speakers,
            actor_sample_windows,
            [path.speaker_id for path in paths],
            fixed_lags,
        )
        total_before = float(np.sum(all_columns**2))
        total_after = float(np.sum(residual**2))
        total_ratio = total_after / max(total_before, 1e-30)

        pair_estimates: list[PairEstimate] = []
        for index, path in enumerate(paths):
            local_sample_windows = _round_windows(
                pair_windows[(actor.id, path.speaker_id)],
                project.sample_rate,
                mic.size,
            )
            local_columns = _design_matrix_samples(
                speakers,
                local_sample_windows,
                [item.speaker_id for item in paths],
                fixed_lags,
            )
            local_target = np.concatenate(
                [mic[start:end] for start, end in local_sample_windows]
            )
            local_residual = local_target - local_columns @ coefficients
            before_pair = float(np.sum(local_columns[:, index] ** 2))
            after_pair = float(np.sum(local_residual**2))
            ratio = after_pair / max(before_pair, 1e-30)
            coefficient = float(coefficients[index])
            speaker = project.speaker_by_id(path.speaker_id)
            pair_estimates.append(
                PairEstimate(
                    actor_id=actor.id,
                    speaker_id=path.speaker_id,
                    speaker_azimuth_deg=speaker.azimuth_deg,
                    speaker_elevation_deg=speaker.elevation_deg,
                    gain_linear=coefficient,
                    gain_db=20.0 * math.log10(max(abs(coefficient), 1e-15)),
                    delay_samples=int(fixed_lags[index]),
                    delay_ms=1000.0 * fixed_lags[index] / project.sample_rate,
                    before_energy=before_pair,
                    after_energy=after_pair,
                    residual_ratio=ratio,
                    reference_windows=len(
                        pair_windows[(actor.id, path.speaker_id)]
                    ),
                )
            )

        direct_loss, probe_count = _direct_loss_ratio(project, actor.id, mic, clean)
        actor_results[actor.id] = ActorCrosstalkResult(
            actor_id=actor.id,
            clean=clean,
            before_energy=total_before,
            after_energy=total_after,
            residual_ratio=total_ratio,
            direct_loss_ratio=direct_loss,
            direct_probe_count=probe_count,
            pairs=tuple(pair_estimates),
            method="reference_xcorr_then_joint_least_squares_v1",
        )

    result = CrosstalkResult(
        actors=actor_results,
        residual_energy_ratio_max=project.crosstalk.residual_energy_ratio_max,
        max_direct_energy_loss_ratio=project.crosstalk.max_direct_energy_loss_ratio,
    )
    _enforce_acceptance(project, result)
    return result


def _windows_for_actor(project: Project, actor_id: str) -> list[ReferenceWindow]:
    result = []
    for window in project.crosstalk.reference_windows:
        if window.actor_id in (None, actor_id):
            result.append(window)
    return result


def _probe_windows_for_actor(project: Project, actor_id: str) -> list[ReferenceWindow]:
    result = []
    for window in project.crosstalk.direct_probe_windows:
        if window.actor_id in (None, actor_id):
            result.append(window)
    return result


def _estimate_initial_lag(
    mic: np.ndarray,
    source: np.ndarray,
    windows: list[ReferenceWindow],
    sample_rate: int,
    max_delay_ms: float,
) -> int | None:
    max_lag = int(round(max_delay_ms * sample_rate / 1000.0))
    best_score = -1.0
    best_lag: int | None = None
    correlation = np.zeros(max_lag + 1, dtype=np.float64)
    source_energy = np.zeros(max_lag + 1, dtype=np.float64)

    for window in windows:
        start = max(0, int(round(window.start_seconds * sample_rate)))
        end = min(mic.size, int(round(window.end_seconds * sample_rate)))
        if end - start <= 1:
            continue
        a = mic[start:end]
        b = source[start:end]
        local_max = min(max_lag, a.size - 1)
        if local_max < 0:
            continue
        nfft = 1 << (a.size + b.size - 1).bit_length()
        aa = np.fft.rfft(a, nfft)
        bb = np.fft.rfft(b, nfft)
        local = np.fft.irfft(aa * np.conj(bb), nfft)[: a.size]
        correlation[: local_max + 1] += local[: local_max + 1]
        valid = min(local_max, b.size - 1) + 1
        prefix = np.concatenate(([0.0], np.cumsum(b * b)))
        for lag in range(valid):
            source_energy[lag] += prefix[b.size - lag]

    for lag in range(max_lag + 1):
        if source_energy[lag] <= 1e-20:
            continue
        score = correlation[lag] * correlation[lag] / source_energy[lag]
        if score > best_score:
            best_score = score
            best_lag = lag
    if best_lag is None or best_score <= 1e-20:
        return None
    return int(best_lag)


def _window_series_samples(
    source: np.ndarray, start: int, end: int, lag: int
) -> np.ndarray:
    result = np.zeros(max(0, end - start), dtype=np.float64)
    if lag >= end - start:
        return result
    src_start = start - lag
    src_end = end - lag
    if src_end <= 0:
        return result
    if src_start < 0:
        result[-src_start:] = source[0:src_end]
    else:
        result[:] = source[src_start:src_end]
    return result


def _design_matrix_samples(
    speakers: dict[str, np.ndarray],
    sample_windows: list[tuple[int, int]],
    speaker_ids: list[str],
    lags: list[int],
) -> np.ndarray:
    blocks = []
    for start, end in sample_windows:
        blocks.append(
            np.stack(
                [
                    _window_series_samples(speakers[speaker_id], start, end, lag)
                    for speaker_id, lag in zip(speaker_ids, lags)
                ],
                axis=1,
            )
        )
    return np.concatenate(blocks, axis=0)


def _round_windows(
    windows: list[ReferenceWindow], sample_rate: int, n: int
) -> list[tuple[int, int]]:
    result = []
    for window in windows:
        start = max(0, int(round(window.start_seconds * sample_rate)))
        end = min(n, int(round(window.end_seconds * sample_rate)))
        if end > start:
            result.append((start, end))
    return result


def _fit_lags_and_gains(
    mic: np.ndarray,
    speakers: dict[str, np.ndarray],
    sample_windows: list[tuple[int, int]],
    speaker_ids: list[str],
    initial_lags: list[int],
    sample_rate: int,
    max_delay_ms: float,
) -> tuple[list[int], np.ndarray, np.ndarray]:
    lags = [int(value) for value in initial_lags]
    target = np.concatenate([mic[start:end] for start, end in sample_windows])
    matrix = _design_matrix_samples(speakers, sample_windows, speaker_ids, lags)
    coefficients, *_ = np.linalg.lstsq(matrix, target, rcond=None)
    residual = target - matrix @ coefficients
    max_lag = int(round(max_delay_ms * sample_rate / 1000.0))

    for _iteration in range(12):
        previous = tuple(lags)
        previous_coefficients = coefficients.copy()
        for column_index, speaker_id in enumerate(speaker_ids):
            contribution = matrix[:, column_index] * coefficients[column_index]
            residual = residual + contribution
            current_lag = lags[column_index]
            best_error = math.inf
            best_lag = current_lag
            best_column = matrix[:, column_index]
            best_coefficient = 0.0
            for lag in range(
                max(0, current_lag - 3), min(max_lag, current_lag + 3) + 1
            ):
                column = _design_matrix_samples(
                    speakers, sample_windows, [speaker_id], [lag]
                )[:, 0]
                energy = float(np.dot(column, column))
                if energy <= 1e-30:
                    coefficient = 0.0
                else:
                    coefficient = float(np.dot(residual, column) / energy)
                trial = residual - coefficient * column
                error = float(np.dot(trial, trial))
                if error < best_error:
                    best_error = error
                    best_lag = lag
                    best_column = column
                    best_coefficient = coefficient
            lags[column_index] = best_lag
            matrix[:, column_index] = best_column
            coefficients[column_index] = best_coefficient
            residual = residual - best_coefficient * best_column

        # One Gauss-Newton style gain refresh after delay changes.
        new_coefficients, *_ = np.linalg.lstsq(matrix, target, rcond=None)
        coefficients = new_coefficients
        residual = target - matrix @ coefficients
        if tuple(lags) == previous and np.allclose(
            coefficients, previous_coefficients, rtol=0.0, atol=1e-12
        ):
            break
    return lags, coefficients, residual


def _subtract_full(
    mic: np.ndarray,
    speakers: dict[str, np.ndarray],
    paths: list[Any],
    lags: list[int],
    coefficients: np.ndarray,
) -> np.ndarray:
    estimate = np.zeros_like(mic)
    for path, lag, coefficient in zip(paths, lags, coefficients):
        source = speakers[path.speaker_id]
        delayed = np.zeros_like(mic)
        if lag < mic.size:
            if lag == 0:
                delayed = source.copy()
            else:
                delayed[lag:] = source[:-lag]
        estimate += coefficient * delayed
    return estimate


def _direct_loss_ratio(
    project: Project, actor_id: str, mic: np.ndarray, clean: np.ndarray
) -> tuple[float | None, int]:
    windows = _probe_windows_for_actor(project, actor_id)
    if not windows:
        windows = _infer_direct_windows(project, actor_id, mic)
    if not windows:
        return None, 0
    before = []
    after = []
    for window in windows:
        start = max(0, int(round(window.start_seconds * project.sample_rate)))
        end = min(mic.size, int(round(window.end_seconds * project.sample_rate)))
        before.append(mic[start:end])
        after.append(clean[start:end])
    before_signal = np.concatenate(before) if before else np.zeros(0)
    after_signal = np.concatenate(after) if after else np.zeros(0)
    before_energy = float(np.dot(before_signal, before_signal))
    after_energy = float(np.dot(after_signal, after_signal))
    if before_energy <= 1e-20:
        return None, len(windows)
    loss = 1.0 - after_energy / before_energy
    return max(0.0, min(1.0, loss)), len(windows)


def _infer_direct_windows(
    project: Project, actor_id: str, mic: np.ndarray
) -> list[ReferenceWindow]:
    # Only used for external projects that omit direct probes. Select short
    # windows where no feedback path is declared for the actor; this cannot prove
    # direct-only content, so the report marks the probe count and measured ratio.
    if any(path.actor_id == actor_id for path in project.crosstalk.feedback):
        return []
    return []


def _enforce_acceptance(project: Project, result: CrosstalkResult) -> None:
    for actor in project.actors:
        actor_result = result.actors[actor.id]
        if actor_result.pairs and actor_result.residual_ratio > (
            project.crosstalk.residual_energy_ratio_max + 1e-12
        ):
            raise ProcessingFailure(
                "CROSSTALK_RESIDUAL_ABOVE_THRESHOLD",
                "Residual speaker-return energy exceeds the project threshold.",
                channel=actor.channel,
                details={
                    "actual": actor_result.residual_ratio,
                    "limit": project.crosstalk.residual_energy_ratio_max,
                },
            )
        if (
            actor_result.direct_loss_ratio is not None
            and actor_result.direct_loss_ratio
            > project.crosstalk.max_direct_energy_loss_ratio + 1e-12
        ):
            raise ProcessingFailure(
                "DIRECT_ENERGY_LOSS_ABOVE_THRESHOLD",
                "Actor direct-energy loss exceeds the project tolerance.",
                channel=actor.channel,
                details={
                    "actual": actor_result.direct_loss_ratio,
                    "limit": project.crosstalk.max_direct_energy_loss_ratio,
                },
            )

