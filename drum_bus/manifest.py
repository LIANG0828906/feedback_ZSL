from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import ValidationFailure, ValidationIssue
from .wavio import WavData, WavFormatError, read_wav


DEFAULT_TRUE_PEAK_DB = -1.0
DEFAULT_BLOCK_SIZE = 4096
ALGORITHM_VERSION = 1


@dataclass(frozen=True)
class ReflectionWindow:
    start_ms: float
    end_ms: float

    def as_dict(self) -> dict[str, float]:
        return {"start_ms": self.start_ms, "end_ms": self.end_ms}


@dataclass(frozen=True)
class ReflectionSpec:
    delays_ms: tuple[float, ...]
    gains_db: tuple[float, ...]

    def delay_samples(self, sample_rate: int) -> tuple[int, ...]:
        return tuple(int(round(value * sample_rate / 1000.0)) for value in self.delays_ms)

    def gain_linear(self) -> tuple[float, ...]:
        return tuple(10.0 ** (value / 20.0) for value in self.gains_db)


@dataclass(frozen=True)
class ChannelSpec:
    channel: str
    instrument: str
    path: Path
    start_offset_samples: int
    reference_times: tuple[float, ...]
    reflection_window: ReflectionWindow | None
    mix_weight: float
    pan: float
    synth_profile: str
    early_reflections: bool

    def reference_samples(self, sample_rate: int) -> tuple[int, ...]:
        return tuple(int(round(value * sample_rate)) for value in self.reference_times)

    def as_summary(self) -> dict[str, Any]:
        return {
            "channel": self.channel,
            "instrument": self.instrument,
            "path": str(self.path),
            "start_offset_samples": self.start_offset_samples,
            "reference_times": list(self.reference_times),
            "reflection_window": (
                self.reflection_window.as_dict() if self.reflection_window else None
            ),
            "mix_weight": self.mix_weight,
            "pan": self.pan,
            "early_reflections": self.early_reflections,
        }


@dataclass(frozen=True)
class Project:
    manifest_path: Path
    manifest_data: dict[str, Any]
    sample_rate: int
    bit_depth: int
    seed: int
    channels: tuple[ChannelSpec, ...]
    ambient: ChannelSpec
    reflection: ReflectionSpec
    target_lufs: float
    true_peak_db: float
    block_size: int
    wav_data: dict[str, WavData]
    source_sha256: dict[str, str]
    digest: str

    @property
    def all_specs(self) -> tuple[ChannelSpec, ...]:
        return self.channels + (self.ambient,)

    def summary(self) -> dict[str, Any]:
        return {
            "manifest_path": str(self.manifest_path),
            "sample_rate": self.sample_rate,
            "bit_depth": self.bit_depth,
            "seed": self.seed,
            "target_lufs": self.target_lufs,
            "true_peak_db": self.true_peak_db,
            "block_size": self.block_size,
            "channels": [channel.as_summary() for channel in self.channels],
            "ambient": self.ambient.as_summary(),
            "reflection": {
                "delays_ms": list(self.reflection.delays_ms),
                "gains_db": list(self.reflection.gains_db),
            },
        }


def _issue(channel: str, code: str, message: str, path: Path | None = None) -> ValidationIssue:
    return ValidationIssue(
        channel=channel,
        code=code,
        message=message,
        path=str(path) if path is not None else None,
    )


def _parse_window(
    value: Any, channel: str, issues: list[ValidationIssue]
) -> ReflectionWindow | None:
    if value is None:
        return None
    try:
        if isinstance(value, dict):
            start_ms = float(value["start_ms"])
            end_ms = float(value["end_ms"])
        elif isinstance(value, (list, tuple)) and len(value) == 2:
            start_ms = float(value[0])
            end_ms = float(value[1])
        else:
            raise ValueError
    except (KeyError, TypeError, ValueError):
        issues.append(
            _issue(channel, "MANIFEST_INVALID", "reflection_window must be [start_ms, end_ms]")
        )
        return None
    if not (0.0 <= start_ms < end_ms):
        issues.append(
            _issue(
                channel,
                "MANIFEST_INVALID",
                "reflection_window requires 0 <= start_ms < end_ms",
            )
        )
        return None
    return ReflectionWindow(start_ms=start_ms, end_ms=end_ms)


def _parse_channel(
    raw: Any,
    index: int,
    base_dir: Path,
    issues: list[ValidationIssue],
    default_window: ReflectionWindow | None,
) -> ChannelSpec | None:
    fallback_name = f"channel_{index}"
    if not isinstance(raw, dict):
        issues.append(_issue(fallback_name, "MANIFEST_INVALID", "channel must be an object"))
        return None
    channel = str(raw.get("channel", raw.get("name", raw.get("id", fallback_name))))
    instrument = str(raw.get("instrument", raw.get("instrument_name", ""))).strip()
    if not instrument:
        issues.append(_issue(channel, "MANIFEST_INVALID", "instrument name is required"))
        return None
    path_value = raw.get("path", raw.get("file"))
    if not isinstance(path_value, str) or not path_value:
        issues.append(_issue(channel, "MANIFEST_INVALID", "WAV path is required"))
        return None
    path = Path(path_value)
    if not path.is_absolute():
        path = base_dir / path
    path = path.resolve()

    try:
        start_offset = int(raw.get("start_offset_samples", raw.get("start_offset", 0)))
    except (TypeError, ValueError):
        issues.append(
            _issue(channel, "MANIFEST_INVALID", "start_offset_samples must be an integer")
        )
        start_offset = 0

    reference_raw = raw.get("reference_times", raw.get("points", []))
    reference_times: tuple[float, ...] = ()
    if reference_raw is not None:
        try:
            if not isinstance(reference_raw, (list, tuple)):
                raise TypeError
            reference_times = tuple(float(value) for value in reference_raw)
        except (TypeError, ValueError):
            issues.append(
                _issue(channel, "MANIFEST_INVALID", "reference_times must be numeric")
            )
            reference_times = ()
    if any(value < 0.0 for value in reference_times):
        issues.append(
            _issue(channel, "MANIFEST_INVALID", "reference_times cannot be negative")
        )
    if any(
        reference_times[i] >= reference_times[i + 1]
        for i in range(len(reference_times) - 1)
    ):
        issues.append(
            _issue(
                channel,
                "REFERENCE_TIMES_NOT_ASCENDING",
                "reference_times must be strictly ascending",
            )
        )

    window_value = raw.get("reflection_window")
    window = (
        _parse_window(window_value, channel, issues)
        if window_value is not None
        else default_window
    )
    try:
        mix_weight = float(raw.get("mix_weight", 1.0))
        pan = float(raw.get("pan", 0.0))
    except (TypeError, ValueError):
        issues.append(_issue(channel, "MANIFEST_INVALID", "mix_weight/pan must be numeric"))
        mix_weight = 1.0
        pan = 0.0
    if mix_weight < 0.0:
        issues.append(_issue(channel, "MANIFEST_INVALID", "mix_weight cannot be negative"))
    if not -1.0 <= pan <= 1.0:
        issues.append(_issue(channel, "MANIFEST_INVALID", "pan must be inside [-1, 1]"))
    synth_profile = str(raw.get("synth_profile", raw.get("profile", channel)))
    early_raw = raw.get("early_reflections", False)
    if not isinstance(early_raw, bool):
        issues.append(
            _issue(channel, "MANIFEST_INVALID", "early_reflections must be boolean")
        )
    early_reflections = bool(early_raw)
    if early_reflections and window is None:
        issues.append(
            _issue(
                channel,
                "MANIFEST_INVALID",
                "early_reflections requires a reflection_window",
            )
        )
    return ChannelSpec(
        channel=channel,
        instrument=instrument,
        path=path,
        start_offset_samples=start_offset,
        reference_times=reference_times,
        reflection_window=window,
        mix_weight=mix_weight,
        pan=pan,
        synth_profile=synth_profile,
        early_reflections=early_reflections,
    )


def _canonical_digest(
    manifest_data: dict[str, Any], source_sha256: dict[str, str]
) -> str:
    digest = hashlib.sha256()
    digest.update(b"drum-bus-input-v1\0")
    digest.update(
        json.dumps(
            manifest_data,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    )
    for key in sorted(source_sha256):
        digest.update(key.encode("utf-8"))
        digest.update(b"\0")
        digest.update(source_sha256[key].encode("ascii"))
        digest.update(b"\0")
    return digest.hexdigest()


def load_project(manifest_path: str | Path) -> Project:
    manifest_file = Path(manifest_path).resolve()
    issues: list[ValidationIssue] = []
    try:
        manifest_data = json.loads(manifest_file.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise ValidationFailure(
            [_issue("manifest", "FILE_MISSING", "manifest file does not exist", manifest_file)]
        )
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValidationFailure(
            [_issue("manifest", "MANIFEST_INVALID", f"cannot parse manifest: {exc}", manifest_file)]
        )
    if not isinstance(manifest_data, dict):
        raise ValidationFailure(
            [_issue("manifest", "MANIFEST_INVALID", "manifest root must be an object")]
        )

    try:
        sample_rate = int(manifest_data["sample_rate"])
        bit_depth = int(manifest_data["bit_depth"])
        seed = int(manifest_data["seed"])
    except (KeyError, TypeError, ValueError):
        raise ValidationFailure(
            [
                _issue(
                    "manifest",
                    "MANIFEST_INVALID",
                    "sample_rate, bit_depth and integer seed are required",
                )
            ]
        )
    if sample_rate < 8000:
        issues.append(_issue("manifest", "MANIFEST_INVALID", "sample_rate is too low"))
    if bit_depth not in (16, 24, 32):
        issues.append(
            _issue("manifest", "MANIFEST_INVALID", "bit_depth must be 16, 24 or 32")
        )

    master = manifest_data.get("master", {})
    if not isinstance(master, dict):
        master = {}
    if "target_lufs" not in master:
        issues.append(
            _issue("manifest", "MANIFEST_INVALID", "master.target_lufs is required")
        )
    try:
        target_lufs = float(master.get("target_lufs", -16.0))
        true_peak_db = float(master.get("true_peak_db", DEFAULT_TRUE_PEAK_DB))
    except (TypeError, ValueError):
        issues.append(
            _issue("manifest", "MANIFEST_INVALID", "master loudness values must be numeric")
        )
        target_lufs = -16.0
        true_peak_db = DEFAULT_TRUE_PEAK_DB
    if target_lufs > 0.0:
        issues.append(_issue("manifest", "MANIFEST_INVALID", "target_lufs must be <= 0"))
    if true_peak_db > 0.0:
        issues.append(_issue("manifest", "MANIFEST_INVALID", "true_peak_db must be <= 0"))

    render_config = manifest_data.get("render", {})
    if not isinstance(render_config, dict):
        render_config = {}
    try:
        block_size = int(render_config.get("block_size", DEFAULT_BLOCK_SIZE))
    except (TypeError, ValueError):
        block_size = DEFAULT_BLOCK_SIZE
        issues.append(_issue("manifest", "MANIFEST_INVALID", "block_size must be an integer"))
    if block_size < 1:
        issues.append(_issue("manifest", "MANIFEST_INVALID", "block_size must be positive"))

    reflection_raw = manifest_data.get("reflection", {})
    if not isinstance(reflection_raw, dict):
        reflection_raw = {}
    try:
        delays = tuple(float(value) for value in reflection_raw.get("delays_ms", []))
        gains = tuple(float(value) for value in reflection_raw.get("gains_db", []))
    except (TypeError, ValueError):
        issues.append(
            _issue("manifest", "MANIFEST_INVALID", "reflection delays/gains must be numeric")
        )
        delays = ()
        gains = ()
    if len(delays) != len(gains):
        issues.append(
            _issue(
                "manifest",
                "MANIFEST_INVALID",
                "reflection delays_ms and gains_db must have equal lengths",
            )
        )
    if any(value <= 0.0 for value in delays) or any(value >= 0.0 for value in gains):
        issues.append(
            _issue(
                "manifest",
                "MANIFEST_INVALID",
                "reflection delays must be positive and gains must be negative dB",
            )
        )

    default_window = None
    if "reflection_window" in manifest_data:
        default_window = _parse_window(
            manifest_data.get("reflection_window"), "manifest", issues
        )

    channels_raw = manifest_data.get("channels")
    channels: list[ChannelSpec] = []
    if not isinstance(channels_raw, list):
        issues.append(_issue("manifest", "MANIFEST_INVALID", "channels must be an array"))
        channels_raw = []
    for index, raw in enumerate(channels_raw):
        parsed = _parse_channel(raw, index, manifest_file.parent, issues, default_window)
        if parsed is not None:
            channels.append(parsed)
    if len(channels) != 3:
        issues.append(
            _issue(
                "manifest",
                "CHANNEL_COUNT_MISMATCH",
                f"exactly three percussion channels are required, got {len(channels)}",
            )
        )

    ambient_raw = manifest_data.get("ambient")
    ambient: ChannelSpec | None = None
    if isinstance(ambient_raw, dict):
        ambient = _parse_channel(
            ambient_raw, len(channels), manifest_file.parent, issues, default_window
        )
    else:
        issues.append(
            _issue("ambient", "MANIFEST_INVALID", "ambient reference channel is required")
        )
    if ambient is not None and ambient.reference_times:
        issues.append(
            _issue("ambient", "MANIFEST_INVALID", "ambient cannot declare drum reference_times")
        )

    instruments: dict[str, list[str]] = {}
    for channel in channels + ([ambient] if ambient else []):
        instruments.setdefault(channel.instrument, []).append(channel.channel)
    for instrument, names in instruments.items():
        if len(names) > 1:
            for name in names:
                issues.append(
                    _issue(
                        name,
                        "DUPLICATE_INSTRUMENT",
                        f"instrument {instrument!r} is assigned more than once",
                    )
                )

    channel_names: dict[str, list[str]] = {}
    for channel in channels + ([ambient] if ambient else []):
        channel_names.setdefault(channel.channel, []).append(channel.instrument)
    for name, values in channel_names.items():
        if len(values) > 1:
            issues.append(
                _issue(name, "DUPLICATE_CHANNEL_NAME", "channel id is assigned more than once")
            )

    wav_data: dict[str, WavData] = {}
    source_sha256: dict[str, str] = {}
    all_specs = channels + ([ambient] if ambient else [])
    for channel in all_specs:
        if not channel.path.is_file():
            issues.append(
                _issue(
                    channel.channel,
                    "FILE_MISSING",
                    f"WAV file for {channel.instrument!r} does not exist",
                    channel.path,
                )
            )
            continue
        try:
            payload = channel.path.read_bytes()
            source_sha256[channel.channel] = hashlib.sha256(payload).hexdigest()
            wav = read_wav(channel.path)
        except (OSError, WavFormatError) as exc:
            issues.append(
                _issue(
                    channel.channel,
                    "WAV_FORMAT_ERROR",
                    f"cannot read WAV: {exc}",
                    channel.path,
                )
            )
            continue
        if wav.info.sample_rate != sample_rate:
            issues.append(
                _issue(
                    channel.channel,
                    "SAMPLE_RATE_MISMATCH",
                    f"expected {sample_rate} Hz, got {wav.info.sample_rate} Hz",
                    channel.path,
                )
            )
        if wav.info.bit_depth != bit_depth:
            issues.append(
                _issue(
                    channel.channel,
                    "BIT_DEPTH_MISMATCH",
                    f"expected {bit_depth}-bit, got {wav.info.bit_depth}-bit",
                    channel.path,
                )
            )
        if wav.info.frames < 1:
            issues.append(
                _issue(channel.channel, "WAV_FORMAT_ERROR", "WAV contains no frames")
            )
        wav_data[channel.channel] = wav

    if issues:
        raise ValidationFailure(issues)

    assert ambient is not None
    digest = _canonical_digest(manifest_data, source_sha256)
    return Project(
        manifest_path=manifest_file,
        manifest_data=manifest_data,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        seed=seed,
        channels=tuple(channels),
        ambient=ambient,
        reflection=ReflectionSpec(delays_ms=delays, gains_db=gains),
        target_lufs=target_lufs,
        true_peak_db=true_peak_db,
        block_size=block_size,
        wav_data=wav_data,
        source_sha256=source_sha256,
        digest=digest,
    )
