from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .manifest import ProjectSpec


@dataclass(frozen=True)
class ToneComponent:
    harmonic: int
    frequency_hz: float
    cosine_coefficient: float
    sine_coefficient: float

    def as_dict(self) -> dict[str, Any]:
        return {
            "harmonic": self.harmonic,
            "frequency_hz": self.frequency_hz,
            "cosine_coefficient": self.cosine_coefficient,
            "sine_coefficient": self.sine_coefficient,
        }


@dataclass(frozen=True)
class DecouplingEvent:
    source: str
    target: str
    start_sample: int
    end_sample: int
    detected: bool
    noise_floor_rms: float
    components: tuple[ToneComponent, ...]
    before_component_rms: float
    after_component_rms: float
    attenuation_db: float | None
    residual_dbfs: float | None
    unrelated_peak_loss_db: float | None
    samples_modified: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "source": self.source,
            "target": self.target,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "detected": self.detected,
            "noise_floor_rms": self.noise_floor_rms,
            "components": [component.as_dict() for component in self.components],
            "before_component_rms": self.before_component_rms,
            "after_component_rms": self.after_component_rms,
            "attenuation_db": self.attenuation_db,
            "residual_dbfs": self.residual_dbfs,
            "unrelated_peak_loss_db": self.unrelated_peak_loss_db,
            "samples_modified": self.samples_modified,
        }


@dataclass
class DecouplingResult:
    channels: dict[str, np.ndarray]
    events: tuple[DecouplingEvent, ...]
    noise_reference: str
    noise_floor_rms: float
    modified_targets: tuple[str, ...]
    untouched_channels: tuple[str, ...]
    parameter_events: tuple[dict[str, Any], ...]

def estimate_noise_floor(samples: np.ndarray) -> float:
    if samples.size < 8:
        return float(np.sqrt(np.mean(np.square(samples))) + 1e-12)
    differences = np.diff(np.asarray(samples, dtype=np.float64))
    median = float(np.median(differences))
    mad = float(np.median(np.abs(differences - median)))
    sigma = mad / 0.6744897501960817 / math.sqrt(2.0)
    return max(sigma, 1e-12)


def _tone_rms(component: ToneComponent) -> float:
    return math.hypot(component.cosine_coefficient, component.sine_coefficient) / math.sqrt(
        2.0
    )


def _fit_window(
    signal: np.ndarray,
    start: int,
    stop: int,
    sample_rate: int,
    frequency: float,
    harmonics: tuple[int, ...],
    noise_floor: float,
    residual_threshold_dbfs: float,
    detection_margin_db: float,
) -> tuple[tuple[ToneComponent, ...], float, float]:
    count = stop - start
    if count < 16:
        return (), 0.0, 0.0
    time = np.arange(start, stop, dtype=np.float64) / sample_rate
    columns: list[np.ndarray] = []
    for harmonic in harmonics:
        frequency_hz = frequency * harmonic
        if frequency_hz >= sample_rate * 0.45:
            continue
        angle = 2.0 * math.pi * frequency_hz * time
        columns.append(np.cos(angle))
        columns.append(np.sin(angle))
    if not columns:
        return (), 0.0, 0.0
    design = np.column_stack(columns)
    centered = np.asarray(signal[start:stop], dtype=np.float64)
    centered = centered - float(np.mean(centered))
    coefficients, *_ = np.linalg.lstsq(design, centered, rcond=None)
    threshold = max(
        10.0 ** (residual_threshold_dbfs / 20.0),
        noise_floor * 10.0 ** (detection_margin_db / 20.0),
    )
    detected_components: list[ToneComponent] = []
    total_before_power = 0.0
    total_after_power = 0.0
    index = 0
    for harmonic in harmonics:
        frequency_hz = frequency * harmonic
        if frequency_hz >= sample_rate * 0.45:
            continue
        cosine = float(coefficients[index])
        sine = float(coefficients[index + 1])
        index += 2
        component = ToneComponent(harmonic, frequency_hz, cosine, sine)
        rms = _tone_rms(component)
        if rms <= threshold:
            continue
        detected_components.append(component)
        total_before_power += rms * rms

    fitted = np.zeros(count, dtype=np.float64)
    for component in detected_components:
        angle = 2.0 * math.pi * component.frequency_hz * time
        fitted += (
            component.cosine_coefficient * np.cos(angle)
            + component.sine_coefficient * np.sin(angle)
        )
    residual = centered - fitted
    total_after_power = _projected_rms_after(
        residual,
        time,
        sample_rate,
        tuple(component.frequency_hz for component in detected_components),
    )
    return (
        tuple(detected_components),
        math.sqrt(total_before_power),
        math.sqrt(total_after_power),
    )


def _projected_rms_after(
    residual: np.ndarray,
    time: np.ndarray,
    sample_rate: int,
    frequencies: tuple[float, ...],
) -> float:
    power = 0.0
    for frequency in frequencies:
        angle = 2.0 * math.pi * frequency * time
        design = np.column_stack((np.cos(angle), np.sin(angle)))
        coefficients, *_ = np.linalg.lstsq(design, residual, rcond=None)
        rms = math.hypot(float(coefficients[0]), float(coefficients[1])) / math.sqrt(2.0)
        power += rms * rms
    return power


def _apply_components(
    signal: np.ndarray,
    start: int,
    stop: int,
    sample_rate: int,
    components: tuple[ToneComponent, ...],
    enforce_window_only: bool = True,
) -> int:
    count = stop - start
    if count <= 0 or not components:
        return 0
    time = np.arange(start, stop, dtype=np.float64) / sample_rate
    correction = np.zeros(count, dtype=np.float64)
    for component in components:
        angle = 2.0 * math.pi * component.frequency_hz * time
        correction += (
            component.cosine_coefficient * np.cos(angle)
            + component.sine_coefficient * np.sin(angle)
        )
    edge = min(int(round(0.002 * sample_rate)), count // 2)
    if edge > 0:
        ramp = np.linspace(0.0, 1.0, edge, endpoint=False)
        correction[:edge] *= ramp
        correction[-edge:] *= ramp[::-1]
    signal[start:stop] -= correction
    return count


def apply_decoupling_parameters(
    project: ProjectSpec,
    channels: dict[str, np.ndarray],
    parameter_events: tuple[dict[str, Any], ...],
) -> tuple[dict[str, np.ndarray], tuple[DecouplingEvent, ...]]:
    processed = {name: samples.copy() for name, samples in channels.items()}
    events: list[DecouplingEvent] = []
    for event in parameter_events:
        target = str(event["target"])
        components = tuple(
            ToneComponent(
                harmonic=int(item["harmonic"]),
                frequency_hz=float(item["frequency_hz"]),
                cosine_coefficient=float(item["cosine_coefficient"]),
                sine_coefficient=float(item["sine_coefficient"]),
            )
            for item in event.get("components", [])
        )
        start = int(event["start_sample"])
        stop = int(event["end_sample"])
        if components:
            _apply_components(
                processed[target], start, stop, project.sample_rate, components
            )
        events.append(
            DecouplingEvent(
                source=str(event["source"]),
                target=target,
                start_sample=start,
                end_sample=stop,
                detected=bool(event["detected"]),
                noise_floor_rms=float(event["noise_floor_rms"]),
                components=components,
                before_component_rms=float(event.get("before_component_rms", 0.0)),
                after_component_rms=float(event.get("after_component_rms", 0.0)),
                attenuation_db=event.get("attenuation_db"),
                residual_dbfs=event.get("residual_dbfs"),
                unrelated_peak_loss_db=event.get("unrelated_peak_loss_db"),
                samples_modified=int(event.get("samples_modified", 0)),
            )
        )
    return processed, tuple(events)


def decouple_channels(
    project: ProjectSpec,
    channels: dict[str, np.ndarray],
) -> DecouplingResult:
    processed = {name: samples.copy() for name, samples in channels.items()}
    channel_map = project.channel_map
    reference = project.reference_channel.name
    noise_floor = estimate_noise_floor(processed[reference])
    events: list[DecouplingEvent] = []
    parameters: list[dict[str, Any]] = []
    modified_targets: set[str] = set()

    for leak in project.crosstalk:
        start = int(round(leak.start_seconds * project.sample_rate))
        stop = min(
            project.total_samples,
            int(round(leak.end_seconds * project.sample_rate)),
        )
        source_frequency = channel_map[leak.source].frequency_hz
        components, before_rms, after_rms = _fit_window(
            processed[leak.target],
            start,
            stop,
            project.sample_rate,
            source_frequency,
            (1, 2, 3),
            noise_floor,
            project.decoupling.residual_threshold_db,
            project.decoupling.detection_margin_db,
        )
        detected = bool(components)
        samples_modified = 0
        if detected:
            samples_modified = _apply_components(
                processed[leak.target],
                start,
                stop,
                project.sample_rate,
                components,
            )
            modified_targets.add(leak.target)

        residual_components, _, residual_after = _fit_window(
            processed[leak.target],
            start,
            stop,
            project.sample_rate,
            source_frequency,
            (1,),
            noise_floor,
            project.decoupling.residual_threshold_db,
            project.decoupling.detection_margin_db,
        )
        if detected:
            after_component_rms = residual_after
        else:
            residual_components = ()
            after_component_rms = before_rms
        attenuation = None
        if before_rms > 0:
            attenuation = 20.0 * math.log10(
                max(after_component_rms, 1e-15) / before_rms
            )
            attenuation = -attenuation
        residual_dbfs = (
            20.0 * math.log10(max(after_component_rms, 1e-15))
            if after_rms > 0
            else None
        )

        unrelated_peak_loss = _unrelated_peak_loss(
            channels[leak.target],
            processed[leak.target],
            project,
            leak.target,
            start,
            stop,
        )
        event = DecouplingEvent(
            source=leak.source,
            target=leak.target,
            start_sample=start,
            end_sample=stop,
            detected=detected,
            noise_floor_rms=noise_floor,
            components=components,
            before_component_rms=before_rms,
            after_component_rms=after_component_rms,
            attenuation_db=attenuation,
            residual_dbfs=residual_dbfs,
            unrelated_peak_loss_db=unrelated_peak_loss,
            samples_modified=samples_modified,
        )
        events.append(event)
        parameters.append(
            {
                "source": leak.source,
                "target": leak.target,
                "start_sample": start,
                "end_sample": stop,
                "detected": detected,
                "noise_floor_rms": noise_floor,
                "before_component_rms": before_rms,
                "after_component_rms": after_component_rms,
                "attenuation_db": attenuation,
                "residual_dbfs": residual_dbfs,
                "unrelated_peak_loss_db": unrelated_peak_loss,
                "samples_modified": samples_modified,
                "components": [component.as_dict() for component in components],
            }
        )

    all_names = {channel.name for channel in project.channels}
    untouched = tuple(sorted(all_names - modified_targets))
    return DecouplingResult(
        channels=processed,
        events=tuple(events),
        noise_reference=reference,
        noise_floor_rms=noise_floor,
        modified_targets=tuple(sorted(modified_targets)),
        untouched_channels=untouched,
        parameter_events=tuple(parameters),
    )


def _unrelated_peak_loss(
    before: np.ndarray,
    after: np.ndarray,
    project: ProjectSpec,
    target: str,
    leak_start: int,
    leak_stop: int,
) -> float | None:
    candidates: list[int] = []
    for reference_time in project.reference_times:
        point = int(round(reference_time * project.sample_rate))
        if point + int(0.08 * project.sample_rate) < leak_start:
            candidates.append(point)
        elif point - int(0.08 * project.sample_rate) > leak_stop:
            candidates.append(point)
    if not candidates:
        return None
    point = candidates[len(candidates) // 2]
    start = max(0, point - int(0.005 * project.sample_rate))
    stop = min(
        project.total_samples,
        point + int(0.08 * project.sample_rate),
    )
    before_peak = float(np.max(np.abs(before[start:stop])))
    after_peak = float(np.max(np.abs(after[start:stop])))
    if before_peak <= 0:
        return None
    if after_peak <= 0:
        return 120.0
    return max(0.0, 20.0 * math.log10(before_peak / after_peak))
