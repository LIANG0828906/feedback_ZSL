from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import soundfile as sf

from .errors import STRUCTURED_ERROR, BusinessError, StructuredError


SAMPLE_RATE = 48_000
SCENE_NAMES = ("雨滴", "雨幕", "溪流", "虫鸣", "鸟鸣", "雷声", "脚步")
TRACK_NAMES = ("rain_drops", "rain_curtain", "stream", "insects", "birds", "thunder", "footsteps")
_BIT_DEPTHS = {
    "PCM_16": 16,
    "PCM_24": 24,
    "PCM_32": 32,
    "FLOAT": 32,
    "DOUBLE": 64,
}


@dataclass(frozen=True)
class PanMotion:
    depth_radians: float = 0.0
    cycles: float = 0.0
    phase: float = 0.0


@dataclass(frozen=True)
class TrackSpec:
    name: str
    scene_name: str
    path: Path
    channels: int
    bit_depth: int
    surround_weights: tuple[float, float]
    mix_weight: float
    transient_reference_times: tuple[float, ...]
    pan_motion: PanMotion
    synthesis: dict[str, Any]


@dataclass(frozen=True)
class LoopSpec:
    crossfade_seconds: float
    max_seam_sample_delta: float
    seam_energy_length_seconds: float
    endpoint_seal_seconds: float


@dataclass(frozen=True)
class PanSpec:
    stability_threshold_db: float


@dataclass(frozen=True)
class MaskingSpec:
    reference_scene: str
    target_scenes: tuple[str, ...]
    window_before_seconds: float
    window_after_seconds: float
    attack_seconds: float
    release_seconds: float
    residual_energy_ratio: float
    required_snr_improvement_db: float
    detection_min_transient_dbfs: float
    windows: tuple[tuple[float, float], ...] = ()


@dataclass(frozen=True)
class MonoSpec:
    fold_weights: tuple[float, float]
    mid_band_hz: tuple[float, float]
    energy_ratio_tolerance_db: float


@dataclass(frozen=True)
class LoudnessSpec:
    target_lufs: float
    max_abs_deviation_lu: float
    true_peak_limit_dbtp: float


@dataclass(frozen=True)
class RenderingSpec:
    block_frames: int
    snapshot_every_blocks: int


@dataclass(frozen=True)
class Manifest:
    source_path: Path
    raw: dict[str, Any]
    manifest_sha256: str
    seed: int
    sample_rate: int
    duration_seconds: float
    output_bit_depth: int
    loop: LoopSpec
    pan: PanSpec
    masking: MaskingSpec
    mono: MonoSpec
    loudness: LoudnessSpec
    rendering: RenderingSpec
    tracks: tuple[TrackSpec, ...]

    @property
    def frames(self) -> int:
        return int(round(self.duration_seconds * self.sample_rate))

    @property
    def crossfade_frames(self) -> int:
        return int(round(self.loop.crossfade_seconds * self.sample_rate))

    @property
    def source_frames(self) -> int:
        return self.frames + self.crossfade_frames

    @property
    def tracks_by_scene(self) -> dict[str, TrackSpec]:
        return {track.scene_name: track for track in self.tracks}

    def track(self, scene_name: str) -> TrackSpec:
        return self.tracks_by_scene[scene_name]

    def canonical_digest(self) -> str:
        return _sha256_bytes(
            json.dumps(self.raw, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode(
                "utf-8"
            )
        )


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value))


def _number(mapping: dict[str, Any], key: str, default: Any = None) -> float:
    value = mapping.get(key, default)
    if not _is_number(value):
        raise TypeError(f"{key} must be a finite number")
    return float(value)


def _integer(mapping: dict[str, Any], key: str, default: Any = None) -> int:
    value = mapping.get(key, default)
    if not isinstance(value, int) or isinstance(value, bool):
        raise TypeError(f"{key} must be an integer")
    return value


def _weights(value: Any, *, allow_zero: bool = False) -> tuple[float, float]:
    if isinstance(value, dict):
        left = value.get("left")
        right = value.get("right")
    elif isinstance(value, (list, tuple)) and len(value) == 2:
        left, right = value
    else:
        raise TypeError("surround_weights must be [left, right] or {left, right}")
    if not _is_number(left) or not _is_number(right):
        raise TypeError("surround_weights must contain finite numbers")
    left_f, right_f = float(left), float(right)
    if allow_zero:
        if left_f < 0.0 or right_f < 0.0 or left_f + right_f <= 0.0:
            raise ValueError("weights must be non-negative with a positive sum")
    elif left_f <= 0.0 or right_f <= 0.0:
        raise ValueError("surround_weights must both be positive")
    return left_f, right_f


def _track_error(track: dict[str, Any], field: str, message: str) -> StructuredError:
    return StructuredError(
        code=STRUCTURED_ERROR,
        channel=str(track.get("name", "<unknown>")),
        scene=str(track.get("scene_name", "<unknown>")),
        field=field,
        message=message,
    )


def load_manifest(path: str | Path, *, verify_audio: bool = True) -> Manifest:
    manifest_path = Path(path).resolve()
    raw_bytes = manifest_path.read_bytes()
    try:
        raw = json.loads(raw_bytes)
    except Exception as exc:
        raise BusinessError(
            [
                StructuredError(
                    code=STRUCTURED_ERROR,
                    channel="<manifest>",
                    scene="<manifest>",
                    field="json",
                    message=f"cannot parse manifest JSON: {exc}",
                )
            ]
        ) from exc
    if not isinstance(raw, dict):
        raise BusinessError(
            [
                StructuredError(
                    code=STRUCTURED_ERROR,
                    channel="<manifest>",
                    scene="<manifest>",
                    field="root",
                    message="manifest root must be an object",
                )
            ]
        )

    errors: list[StructuredError] = []

    def global_error(field: str, message: str) -> None:
        errors.append(
            StructuredError(
                code=STRUCTURED_ERROR,
                channel="<manifest>",
                scene="<manifest>",
                field=field,
                message=message,
            )
        )

    try:
        seed = _integer(raw, "seed")
    except Exception as exc:
        global_error("seed", str(exc))
        seed = 0

    try:
        sample_rate = _integer(raw, "sample_rate", SAMPLE_RATE)
    except Exception as exc:
        global_error("sample_rate", str(exc))
        sample_rate = SAMPLE_RATE
    if sample_rate != SAMPLE_RATE:
        global_error("sample_rate", f"project sample rate must be {SAMPLE_RATE}, got {sample_rate}")

    try:
        duration_seconds = _number(raw, "duration_seconds")
    except Exception as exc:
        global_error("duration_seconds", str(exc))
        duration_seconds = 0.0
    if duration_seconds <= 0.0:
        global_error("duration_seconds", "duration_seconds must be positive")

    try:
        output_bit_depth = _integer(raw, "output_bit_depth", 32)
    except Exception as exc:
        global_error("output_bit_depth", str(exc))
        output_bit_depth = 32

    loop_raw = raw.get("loop", {})
    pan_raw = raw.get("panning", {})
    masking_raw = raw.get("masking", {})
    mono_raw = raw.get("mono", {})
    loudness_raw = raw.get("loudness", {})
    rendering_raw = raw.get("rendering", {})
    for name, value in (
        ("loop", loop_raw),
        ("panning", pan_raw),
        ("masking", masking_raw),
        ("mono", mono_raw),
        ("loudness", loudness_raw),
        ("rendering", rendering_raw),
    ):
        if not isinstance(value, dict):
            global_error(name, f"{name} must be an object")

    try:
        loop = LoopSpec(
            crossfade_seconds=_number(loop_raw, "crossfade_seconds"),
            max_seam_sample_delta=_number(loop_raw, "max_seam_sample_delta"),
            seam_energy_length_seconds=_number(loop_raw, "seam_energy_length_seconds", 0.02),
            endpoint_seal_seconds=_number(loop_raw, "endpoint_seal_seconds", 0.02),
        )
    except Exception as exc:
        global_error("loop", str(exc))
        loop = LoopSpec(0.0, 0.0, 0.02, 0.02)
    if loop.crossfade_seconds <= 0.0:
        global_error("loop.crossfade_seconds", "crossfade duration must be positive")
    if loop.crossfade_seconds > duration_seconds:
        global_error("loop.crossfade_seconds", "crossfade duration must not exceed project duration")
    if loop.max_seam_sample_delta < 0.0:
        global_error("loop.max_seam_sample_delta", "seam sample delta threshold must be non-negative")
    if loop.seam_energy_length_seconds <= 0.0 or loop.endpoint_seal_seconds < 0.0:
        global_error("loop", "energy window must be positive and endpoint seal must be non-negative")

    try:
        pan = PanSpec(stability_threshold_db=_number(pan_raw, "stability_threshold_db"))
    except Exception as exc:
        global_error("panning", str(exc))
        pan = PanSpec(0.0)
    if pan.stability_threshold_db <= 0.0:
        global_error("panning.stability_threshold_db", "stability threshold must be positive")

    target_scenes_raw = masking_raw.get("target_scenes", ["鸟鸣"])
    if not isinstance(target_scenes_raw, list) or not all(isinstance(item, str) for item in target_scenes_raw):
        global_error("masking.target_scenes", "target_scenes must be a list of scene names")
        target_scenes_raw = []
    try:
        windows_raw = masking_raw.get("windows", [])
        parsed_windows: list[tuple[float, float]] = []
        if not isinstance(windows_raw, list):
            raise TypeError("masking.windows must be a list of [start, end] pairs")
        for item in windows_raw:
            if (
                not isinstance(item, (list, tuple))
                or len(item) != 2
                or not _is_number(item[0])
                or not _is_number(item[1])
            ):
                raise TypeError("each masking window must be a numeric [start, end] pair")
            window_start, window_end = float(item[0]), float(item[1])
            if not (0.0 <= window_start < window_end <= duration_seconds):
                raise ValueError("masking windows must be ordered inside the project duration")
            parsed_windows.append((window_start, window_end))
        parsed_windows.sort()
        masking = MaskingSpec(
            reference_scene=str(masking_raw.get("reference_scene", "雨幕")),
            target_scenes=tuple(target_scenes_raw),
            window_before_seconds=_number(masking_raw, "window_before_seconds", 0.06),
            window_after_seconds=_number(masking_raw, "window_after_seconds", 0.28),
            attack_seconds=_number(masking_raw, "attack_seconds", 0.01),
            release_seconds=_number(masking_raw, "release_seconds", 0.03),
            residual_energy_ratio=_number(masking_raw, "residual_energy_ratio"),
            required_snr_improvement_db=_number(masking_raw, "required_snr_improvement_db", 6.0),
            detection_min_transient_dbfs=_number(masking_raw, "detection_min_transient_dbfs", -55.0),
            windows=tuple(parsed_windows),
        )
    except Exception as exc:
        global_error("masking", str(exc))
        masking = MaskingSpec("雨幕", (), 0.06, 0.28, 0.01, 0.03, 1.0, 6.0, -55.0, ())
    if not (0.0 < masking.residual_energy_ratio <= 1.0):
        global_error("masking.residual_energy_ratio", "must be in (0, 1]")
    if masking.window_before_seconds < 0.0 or masking.window_after_seconds < 0.0:
        global_error("masking.window", "masking window offsets must be non-negative")
    if masking.attack_seconds < 0.0 or masking.release_seconds < 0.0:
        global_error("masking.ramp", "masking ramps must be non-negative")
    if masking.reference_scene not in SCENE_NAMES:
        global_error("masking.reference_scene", "reference scene is not one of the seven required scenes")
    for scene in masking.target_scenes:
        if scene not in SCENE_NAMES:
            global_error("masking.target_scenes", f"unknown target scene: {scene}")

    try:
        fold_value = mono_raw.get("fold_weights", [0.5, 0.5])
        fold_weights = _weights(fold_value, allow_zero=True)
        band = mono_raw.get("mid_band_hz", [300.0, 5000.0])
        if not isinstance(band, (list, tuple)) or len(band) != 2:
            raise TypeError("mid_band_hz must contain two frequencies")
        mid_band = (float(band[0]), float(band[1]))
        mono = MonoSpec(
            fold_weights=fold_weights,
            mid_band_hz=mid_band,
            energy_ratio_tolerance_db=_number(mono_raw, "energy_ratio_tolerance_db", 0.5),
        )
    except Exception as exc:
        global_error("mono", str(exc))
        mono = MonoSpec((0.5, 0.5), (300.0, 5000.0), 0.5)
    if not (0.0 < mono.mid_band_hz[0] < mono.mid_band_hz[1] < sample_rate / 2):
        global_error("mono.mid_band_hz", "mid-band must be ordered and below Nyquist")
    if mono.energy_ratio_tolerance_db < 0.0:
        global_error("mono.energy_ratio_tolerance_db", "tolerance must be non-negative")

    try:
        loudness = LoudnessSpec(
            target_lufs=_number(loudness_raw, "target_lufs"),
            max_abs_deviation_lu=_number(loudness_raw, "max_abs_deviation_lu", 0.5),
            true_peak_limit_dbtp=_number(loudness_raw, "true_peak_limit_dbtp", -1.0),
        )
    except Exception as exc:
        global_error("loudness", str(exc))
        loudness = LoudnessSpec(-18.0, 0.5, -1.0)
    if loudness.max_abs_deviation_lu < 0.0:
        global_error("loudness.max_abs_deviation_lu", "loudness tolerance must be non-negative")
    if loudness.true_peak_limit_dbtp >= 0.0:
        global_error("loudness.true_peak_limit_dbtp", "true peak limit must be below 0 dBTP")

    try:
        rendering = RenderingSpec(
            block_frames=_integer(rendering_raw, "block_frames", 48_000),
            snapshot_every_blocks=_integer(rendering_raw, "snapshot_every_blocks", 1),
        )
    except Exception as exc:
        global_error("rendering", str(exc))
        rendering = RenderingSpec(48_000, 1)
    if rendering.block_frames <= 0 or rendering.snapshot_every_blocks <= 0:
        global_error("rendering", "block size and snapshot interval must be positive")

    tracks_raw = raw.get("tracks")
    if not isinstance(tracks_raw, list):
        global_error("tracks", "tracks must be a list")
        tracks_raw = []
    if len(tracks_raw) != 7:
        global_error("tracks", f"exactly seven mono tracks are required, got {len(tracks_raw)}")

    tracks: list[TrackSpec] = []
    seen_scenes: set[str] = set()
    for index, item in enumerate(tracks_raw):
        if not isinstance(item, dict):
            global_error(f"tracks[{index}]", "track entry must be an object")
            continue
        name = str(item.get("name", f"track_{index}"))
        scene = str(item.get("scene_name", "<missing>"))
        if scene not in SCENE_NAMES:
            errors.append(_track_error(item, "scene_name", f"unknown soundscape name: {scene}"))
        if scene in seen_scenes:
            errors.append(_track_error(item, "scene_name", f"duplicate soundscape name: {scene}"))
        seen_scenes.add(scene)

        times_raw = item.get("transient_reference_times", [])
        times: list[float] = []
        if not isinstance(times_raw, list) or not all(_is_number(value) for value in times_raw):
            errors.append(
                _track_error(item, "transient_reference_times", "reference times must be a numeric list")
            )
        else:
            times = [float(value) for value in times_raw]
            if any(b <= a for a, b in zip(times, times[1:])):
                errors.append(
                    _track_error(
                        item,
                        "transient_reference_times",
                        "reference times must be strictly ascending",
                    )
                )
            if any(value < 0.0 or value > duration_seconds for value in times):
                errors.append(
                    _track_error(
                        item,
                        "transient_reference_times",
                        "reference times must lie within the project duration",
                    )
                )

        try:
            weights = _weights(item.get("surround_weights"))
        except Exception as exc:
            errors.append(_track_error(item, "surround_weights", str(exc)))
            weights = (0.5, 0.5)
        try:
            mix_weight = _number(item, "mix_weight", 0.0)
        except Exception as exc:
            errors.append(_track_error(item, "mix_weight", str(exc)))
            mix_weight = 0.0
        if mix_weight < 0.0:
            errors.append(_track_error(item, "mix_weight", "mix_weight must be non-negative"))
        try:
            channels = _integer(item, "channels", 1)
        except Exception as exc:
            errors.append(_track_error(item, "channels", str(exc)))
            channels = 1
        try:
            declared_track_rate = _integer(item, "sample_rate", SAMPLE_RATE)
        except Exception as exc:
            errors.append(_track_error(item, "sample_rate", str(exc)))
            declared_track_rate = SAMPLE_RATE
        if declared_track_rate != SAMPLE_RATE:
            errors.append(
                _track_error(
                    item,
                    "sample_rate",
                    f"manifest stem sample rate must be {SAMPLE_RATE}, got {declared_track_rate}",
                )
            )
        if channels != 1:
            errors.append(_track_error(item, "channels", f"stem must be mono, got {channels} channels"))
        try:
            bit_depth = _integer(item, "bit_depth", output_bit_depth)
        except Exception as exc:
            errors.append(_track_error(item, "bit_depth", str(exc)))
            bit_depth = output_bit_depth
        if bit_depth not in (16, 24, 32, 64):
            errors.append(_track_error(item, "bit_depth", f"unsupported bit depth: {bit_depth}"))

        motion_raw = item.get("pan_motion", {})
        if not isinstance(motion_raw, dict):
            errors.append(_track_error(item, "pan_motion", "pan_motion must be an object"))
            motion_raw = {}
        try:
            motion = PanMotion(
                depth_radians=_number(motion_raw, "depth_radians", 0.0),
                cycles=_number(motion_raw, "cycles", 0.0),
                phase=_number(motion_raw, "phase", 0.0),
            )
        except Exception as exc:
            errors.append(_track_error(item, "pan_motion", str(exc)))
            motion = PanMotion()
        if motion.depth_radians < 0.0 or motion.cycles < 0.0:
            errors.append(_track_error(item, "pan_motion", "depth and cycles must be non-negative"))

        synthesis = item.get("synthesis", {})
        if not isinstance(synthesis, dict):
            errors.append(_track_error(item, "synthesis", "synthesis must be an object"))
            synthesis = {}
        raw_path = item.get("path")
        if not isinstance(raw_path, str) or not raw_path:
            errors.append(_track_error(item, "path", "stem path is required"))
            stem_path = manifest_path.parent / "<missing>"
        else:
            stem_path = (manifest_path.parent / raw_path).resolve()

        tracks.append(
            TrackSpec(
                name=name,
                scene_name=scene,
                path=stem_path,
                channels=channels,
                bit_depth=bit_depth,
                surround_weights=weights,
                mix_weight=mix_weight,
                transient_reference_times=tuple(times),
                pan_motion=motion,
                synthesis=synthesis,
            )
        )

    missing_scenes = [name for name in SCENE_NAMES if name not in seen_scenes]
    if missing_scenes:
        global_error("tracks", f"missing required soundscape tracks: {', '.join(missing_scenes)}")

    expected_source_frames = int(round(duration_seconds * SAMPLE_RATE)) + int(
        round(loop.crossfade_seconds * SAMPLE_RATE)
    )
    if verify_audio:
        for track in tracks:
            if not track.path.is_file():
                errors.append(
                    StructuredError(
                        code=STRUCTURED_ERROR,
                        channel=track.name,
                        scene=track.scene_name,
                        field="path",
                        message=f"stem file is missing: {track.path}",
                    )
                )
                continue
            try:
                info = sf.info(str(track.path))
            except Exception as exc:
                errors.append(
                    StructuredError(
                        code=STRUCTURED_ERROR,
                        channel=track.name,
                        scene=track.scene_name,
                        field="path",
                        message=f"cannot read stem file: {exc}",
                    )
                )
                continue
            if info.samplerate != SAMPLE_RATE:
                errors.append(
                    StructuredError(
                        code=STRUCTURED_ERROR,
                        channel=track.name,
                        scene=track.scene_name,
                        field="sample_rate",
                        message=f"actual sample rate must be {SAMPLE_RATE}, got {info.samplerate}",
                    )
                )
            if info.channels != track.channels:
                errors.append(
                    StructuredError(
                        code=STRUCTURED_ERROR,
                        channel=track.name,
                        scene=track.scene_name,
                        field="channels",
                        message=f"actual channel count is {info.channels}, manifest declares {track.channels}",
                    )
                )
            actual_bits = _BIT_DEPTHS.get(info.subtype)
            if actual_bits != track.bit_depth:
                errors.append(
                    StructuredError(
                        code=STRUCTURED_ERROR,
                        channel=track.name,
                        scene=track.scene_name,
                        field="bit_depth",
                        message=(
                            f"actual bit depth/subtype is {actual_bits}/{info.subtype}, "
                            f"manifest declares {track.bit_depth}"
                        ),
                    )
                )
            if info.frames < expected_source_frames:
                errors.append(
                    StructuredError(
                        code=STRUCTURED_ERROR,
                        channel=track.name,
                        scene=track.scene_name,
                        field="frames",
                        message=(
                            "stem must contain the complete loop plus crossfade tail "
                            f"(at least {expected_source_frames} frames)"
                        ),
                    )
                )

    if errors:
        raise BusinessError(errors)

    return Manifest(
        source_path=manifest_path,
        raw=raw,
        manifest_sha256=hashlib.sha256(raw_bytes).hexdigest(),
        seed=seed,
        sample_rate=sample_rate,
        duration_seconds=duration_seconds,
        output_bit_depth=output_bit_depth,
        loop=loop,
        pan=pan,
        masking=masking,
        mono=mono,
        loudness=loudness,
        rendering=rendering,
        tracks=tuple(tracks),
    )


def pan_theta(track: TrackSpec, frame_index: int, project_frames: int, sample_rate: int) -> float:
    left_weight, right_weight = track.surround_weights
    base = math.atan2(math.sqrt(right_weight), math.sqrt(left_weight))
    seconds = frame_index / sample_rate
    duration = project_frames / sample_rate
    offset = 0.0
    if track.pan_motion.depth_radians and track.pan_motion.cycles:
        offset = track.pan_motion.depth_radians * math.sin(
            2.0 * math.pi * track.pan_motion.cycles * seconds / duration + track.pan_motion.phase
        )
    low, high = 0.12, math.pi / 2.0 - 0.12
    return min(high, max(low, base + offset))


def pan_gains(
    track: TrackSpec,
    frame_indices: Any,
    project_frames: int,
    sample_rate: int,
) -> tuple[Any, Any]:
    import numpy as np

    left_weight, right_weight = track.surround_weights
    base = math.atan2(math.sqrt(right_weight), math.sqrt(left_weight))
    seconds = np.asarray(frame_indices, dtype=np.float64) / sample_rate
    duration = project_frames / sample_rate
    offset = np.zeros_like(seconds)
    if track.pan_motion.depth_radians and track.pan_motion.cycles:
        offset = track.pan_motion.depth_radians * np.sin(
            2.0 * np.pi * track.pan_motion.cycles * seconds / duration + track.pan_motion.phase
        )
    theta = np.clip(base + offset, 0.12, np.pi / 2.0 - 0.12)
    return np.cos(theta), np.sin(theta)
