"""Manifest (cue sheet) loading and strict, structured validation."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass

from .errors import BusinessCode, StageNoiseError


SUPPORTED_SUBTYPES = {"PCM_16": 16, "PCM_24": 24, "PCM_32": 32}


@dataclass(frozen=True)
class MicSpec:
    name: str
    path: str
    sample_rate: int
    channels: int
    bit_depth: int
    subtype: str
    position: str
    seed: int
    downmix: tuple[float, float]


@dataclass(frozen=True)
class ActionSegment:
    segment_id: str
    start_time: float
    end_time: float
    start_sample: int
    end_sample: int
    mechanism: str
    position_gains: dict


@dataclass(frozen=True)
class Manifest:
    raw: dict
    source_dir: str
    seed: int
    duration_seconds: float
    sample_rate: int
    mics: tuple[MicSpec, ...]
    actions: tuple[ActionSegment, ...]
    noise_band_hz: tuple[float, float]
    center_frequency_tolerance_hz: float
    timing_tolerance_ms: float
    max_false_action_segments: int
    residual_threshold_dbfs: float
    min_attenuation_db: float
    max_content_loss_db: float
    target_loudness_lufs: float
    true_peak_limit_dbtp: float
    chunk_samples: int
    injection_margin_ms: float

    @property
    def total_samples(self) -> int:
        return int(round(self.duration_seconds * self.sample_rate))

    def mic_paths(self) -> list:
        return [os.path.join(self.source_dir, m.path) for m in self.mics]


def _require(mapping, key, context, mic=None):
    if not isinstance(mapping, dict) or key not in mapping:
        raise StageNoiseError(
            BusinessCode.INVALID_MANIFEST,
            f"missing required field '{key}' in {context}", mic=mic)
    return mapping[key]


def _number(value, field_name, *, positive=False, non_negative=False,
            mic=None, integer=False):
    if integer:
        if not isinstance(value, int) or isinstance(value, bool):
            raise StageNoiseError(
                BusinessCode.INVALID_MANIFEST,
                f"field '{field_name}' must be an integer, got {value!r}",
                mic=mic)
    elif not isinstance(value, (int, float)) or isinstance(value, bool):
        raise StageNoiseError(
            BusinessCode.INVALID_MANIFEST,
            f"field '{field_name}' must be a number, got {value!r}", mic=mic)
    if positive and value <= 0:
        raise StageNoiseError(
            BusinessCode.INVALID_MANIFEST,
            f"field '{field_name}' must be positive, got {value!r}", mic=mic)
    if non_negative and value < 0:
        raise StageNoiseError(
            BusinessCode.INVALID_MANIFEST,
            f"field '{field_name}' must be non-negative, got {value!r}",
            mic=mic)
    return value


def load_manifest(manifest_path: str) -> Manifest:
    try:
        with open(manifest_path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError:
        raise StageNoiseError(
            BusinessCode.FILE_MISSING,
            f"manifest not found: {os.path.basename(manifest_path)}")
    except (json.JSONDecodeError, OSError) as exc:
        raise StageNoiseError(
            BusinessCode.INVALID_MANIFEST,
            f"manifest is not valid JSON: {exc}")
    if not isinstance(raw, dict):
        raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                              "manifest root must be a JSON object")
    source_dir = os.path.dirname(os.path.abspath(manifest_path))
    return _build_manifest(raw, source_dir)


def _build_manifest(raw: dict, source_dir: str) -> Manifest:
    seed = _number(_require(raw, "seed", "manifest"), "seed",
                   non_negative=True, integer=True)
    duration = _number(_require(raw, "duration_seconds", "manifest"),
                       "duration_seconds", positive=True)
    sample_rate = _number(_require(raw, "sample_rate", "manifest"),
                          "sample_rate", positive=True, integer=True)

    band = _require(raw, "noise_reference_band_hz", "manifest")
    if (not isinstance(band, (list, tuple)) or len(band) != 2
            or any(not isinstance(x, (int, float)) for x in band)):
        raise StageNoiseError(BusinessCode.INVALID_FREQUENCY_BAND,
                              "noise_reference_band_hz must be [low, high]")
    band_low, band_high = float(band[0]), float(band[1])
    if not (0 < band_low < band_high < sample_rate / 2):
        raise StageNoiseError(
            BusinessCode.INVALID_FREQUENCY_BAND,
            f"illegal reference band [{band_low}, {band_high}] Hz",
            details={"nyquist_hz": sample_rate / 2})

    tol = _number(raw.get("center_frequency_tolerance_hz", 5.0),
                  "center_frequency_tolerance_hz", positive=True)
    timing_tol = _number(raw.get("timing_tolerance_ms", 50.0),
                         "timing_tolerance_ms", positive=True)
    max_false = _number(raw.get("max_false_action_segments", 1),
                        "max_false_action_segments", non_negative=True,
                        integer=True)
    residual = _number(raw.get("residual_threshold_dbfs", -55.0),
                       "residual_threshold_dbfs")
    min_atten = _number(raw.get("min_attenuation_db", 18.0),
                        "min_attenuation_db", positive=True)
    max_loss = _number(raw.get("max_content_loss_db", 1.0),
                       "max_content_loss_db", positive=True)
    target_loud = _number(raw.get("target_loudness_lufs", -23.0),
                          "target_loudness_lufs")
    tp_limit = _number(raw.get("true_peak_limit_dbtp", -1.0),
                       "true_peak_limit_dbtp")
    chunk = _number(raw.get("chunk_samples", 24000), "chunk_samples",
                    positive=True, integer=True)
    margin = _number(raw.get("injection_margin_ms", 150.0),
                     "injection_margin_ms", positive=True)

    mics = _build_mics(raw, sample_rate)
    actions = _build_actions(raw, sample_rate, duration, mics)

    return Manifest(
        raw=raw, source_dir=source_dir, seed=seed,
        duration_seconds=float(duration), sample_rate=int(sample_rate),
        mics=mics, actions=actions,
        noise_band_hz=(band_low, band_high),
        center_frequency_tolerance_hz=float(tol),
        timing_tolerance_ms=float(timing_tol),
        max_false_action_segments=int(max_false),
        residual_threshold_dbfs=float(residual),
        min_attenuation_db=float(min_atten),
        max_content_loss_db=float(max_loss),
        target_loudness_lufs=float(target_loud),
        true_peak_limit_dbtp=float(tp_limit),
        chunk_samples=int(chunk),
        injection_margin_ms=float(margin))


def _build_mics(raw, sample_rate) -> tuple:
    mic_list = _require(raw, "microphones", "manifest")
    if not isinstance(mic_list, list) or not mic_list:
        raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                              "microphones must be a non-empty list")
    seen_names = set()
    mics = []
    for index, item in enumerate(mic_list):
        if not isinstance(item, dict):
            raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                                  f"microphone entry {index} must be an object")
        name = _require(item, "name", "microphone entry")
        if not isinstance(name, str) or not name:
            raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                                  "microphone name must be a non-empty string",
                                  mic=str(name))
        if name in seen_names:
            raise StageNoiseError(BusinessCode.DUPLICATE_MIC_NAME,
                                  f"duplicate microphone name '{name}'",
                                  mic=name)
        seen_names.add(name)
        path = _require(item, "path", "microphone entry", mic=name)
        if not isinstance(path, str) or not path:
            raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                                  "microphone path must be a non-empty string",
                                  mic=name)
        sr = _number(_require(item, "sample_rate", "microphone", mic=name),
                     "sample_rate", positive=True, integer=True, mic=name)
        if int(sr) != int(sample_rate):
            raise StageNoiseError(
                BusinessCode.SAMPLE_RATE_MISMATCH,
                f"mic sample rate {sr} != manifest rate {sample_rate}",
                mic=name, details={"declared_hz": sr,
                                   "manifest_hz": sample_rate})
        channels = _number(_require(item, "channels", "microphone",
                                    mic=name), "channels", positive=True,
                           integer=True, mic=name)
        if int(channels) != 1:
            raise StageNoiseError(
                BusinessCode.CHANNEL_COUNT_MISMATCH,
                f"only mono (1 channel) mics are supported, got {channels}",
                mic=name, details={"declared_channels": channels})
        bit_depth = _number(_require(item, "bit_depth", "microphone",
                                     mic=name), "bit_depth", positive=True,
                            integer=True, mic=name)
        subtype = item.get("subtype", f"PCM_{bit_depth}")
        if subtype not in SUPPORTED_SUBTYPES or \
                SUPPORTED_SUBTYPES[subtype] != int(bit_depth):
            raise StageNoiseError(
                BusinessCode.BIT_DEPTH_MISMATCH,
                f"unsupported bit depth/subtype {bit_depth}/{subtype}",
                mic=name, details={"declared_bit_depth": bit_depth,
                                   "subtype": subtype})
        position = _require(item, "position", "microphone entry", mic=name)
        if not isinstance(position, str) or not position:
            raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                                  "position must be a non-empty string",
                                  mic=name)
        mic_seed = _number(item.get("seed", index), "seed",
                           non_negative=True, integer=True, mic=name)
        weights = item.get("downmix_weight", [1.0, 1.0])
        if (not isinstance(weights, (list, tuple)) or len(weights) != 2
                or any(not isinstance(w, (int, float)) for w in weights)):
            raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                                  "downmix_weight must be [left, right]",
                                  mic=name)
        mics.append(MicSpec(
            name=name, path=path, sample_rate=int(sr),
            channels=int(channels), bit_depth=int(bit_depth),
            subtype=subtype, position=position, seed=int(mic_seed),
            downmix=(float(weights[0]), float(weights[1]))))
    return tuple(mics)


def _build_actions(raw, sample_rate, duration, mics) -> tuple:
    action_list = _require(raw, "action_segments", "manifest")
    if not isinstance(action_list, list) or not action_list:
        raise StageNoiseError(BusinessCode.INVALID_MANIFEST,
                              "action_segments must be a non-empty list")
    known_positions = {m.position for m in mics}
    previous_end = -1.0
    actions = []
    for index, item in enumerate(action_list):
        if not isinstance(item, dict):
            raise StageNoiseError(BusinessCode.INVALID_ACTION_SEGMENT,
                                  f"action entry {index} must be an object")
        seg_id = str(item.get("segment_id", f"action_{index:02d}"))
        start = _number(_require(item, "start_time", "action segment",
                                 mic=seg_id), "start_time",
                        non_negative=True, mic=seg_id)
        end = _number(_require(item, "end_time", "action segment",
                               mic=seg_id), "end_time", mic=seg_id)
        if not (start < end):
            raise StageNoiseError(
                BusinessCode.INVALID_ACTION_SEGMENT,
                f"segment {seg_id}: start {start} >= end {end}",
                mic=seg_id)
        if end > float(duration):
            raise StageNoiseError(
                BusinessCode.INVALID_ACTION_SEGMENT,
                f"segment {seg_id} ends after recording duration",
                mic=seg_id, details={"end_time": end,
                                     "duration_seconds": duration})
        if start < previous_end:
            raise StageNoiseError(
                BusinessCode.INVALID_ACTION_SEGMENT,
                f"segment {seg_id} not strictly ordered "
                f"(start {start} < previous end {previous_end})",
                mic=seg_id)
        previous_end = end
        gains = item.get("position_gains", {})
        if not isinstance(gains, dict) or \
                any(not isinstance(v, (int, float)) or v < 0
                    for v in gains.values()):
            raise StageNoiseError(
                BusinessCode.INVALID_ACTION_SEGMENT,
                f"segment {seg_id}: position_gains must map position->gain",
                mic=seg_id)
        unknown = set(gains) - known_positions
        if unknown:
            raise StageNoiseError(
                BusinessCode.INVALID_ACTION_SEGMENT,
                f"segment {seg_id}: unknown positions {sorted(unknown)}",
                mic=seg_id)
        actions.append(ActionSegment(
            segment_id=seg_id, start_time=float(start), end_time=float(end),
            start_sample=int(round(start * sample_rate)),
            end_sample=int(round(end * sample_rate)),
            mechanism=str(item.get("mechanism", "stage_machinery")),
            position_gains={k: float(v) for k, v in gains.items()}))
    return tuple(actions)
