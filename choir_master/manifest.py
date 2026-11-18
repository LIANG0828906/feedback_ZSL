"""Manifest schema, validation, and deterministic input digests."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import soundfile as sf

from .errors import BusinessError, EXIT_VALIDATION, fail, issue


VOICE_NAMES: tuple[str, ...] = ("女高", "女低", "男高", "男低")
VOICE_ALIASES = {
    "女高": "女高",
    "soprano": "女高",
    "女低": "女低",
    "alto": "女低",
    "男高": "男高",
    "tenor": "男高",
    "男低": "男低",
    "bass": "男低",
}


@dataclass(frozen=True)
class VoiceSpec:
    name: str
    target_energy_ratio: float
    band_low_hz: float
    band_high_hz: float
    pan: float
    synthesis: dict[str, Any]


@dataclass(frozen=True)
class MicSpec:
    name: str
    voice: str
    relative_path: str
    resolved_path: Path
    channels: int
    delay_ms: float
    gain_db: float
    downmix_weight: float


@dataclass
class Project:
    manifest_path: Path
    manifest: dict[str, Any]
    voices: dict[str, VoiceSpec]
    microphones: list[MicSpec]
    sample_rate: int
    bit_depth: int
    seed: int
    duration_seconds: float
    target_loudness_lufs: float
    true_peak_limit_dbtp: float
    block_frames: int

    def mics_for_voice(self, voice: str) -> list[MicSpec]:
        return [mic for mic in self.microphones if mic.voice == voice]


def load_manifest_document(path: str | Path) -> tuple[Path, dict[str, Any]]:
    manifest_path = Path(path).expanduser().resolve()
    if not manifest_path.is_file():
        fail(
            "E_MANIFEST_MISSING",
            "<manifest>",
            "manifest file does not exist",
        )
    try:
        with open(manifest_path, "r", encoding="utf-8") as handle:
            document = json.load(handle)
    except (OSError, UnicodeError) as exc:
        fail(
            "E_MANIFEST_UNREADABLE",
            "<manifest>",
            f"manifest cannot be read: {type(exc).__name__}",
        )
    except json.JSONDecodeError as exc:
        fail(
            "E_MANIFEST_JSON",
            "<manifest>",
            f"manifest is not valid JSON at line {exc.lineno}",
        )
    if not isinstance(document, dict):
        fail("E_MANIFEST_SCHEMA", "<manifest>", "manifest root must be an object")
    return manifest_path, document


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def manifest_digest(document: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json_bytes(document)).hexdigest()


def _json_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    if not math.isfinite(number):
        return None
    return number


def _subtype_bit_depth(subtype: str) -> int | None:
    return {
        "PCM_U8": 8,
        "PCM_16": 16,
        "PCM_24": 24,
        "PCM_32": 32,
        "FLOAT": 32,
        "DOUBLE": 64,
    }.get(subtype)


def load_project(path: str | Path, *, check_files: bool = True) -> Project:
    manifest_path, document = load_manifest_document(path)
    issues = []

    def add(code: str, mic: str, message: str, **kwargs: Any) -> None:
        issues.append(issue(code, mic, message, **kwargs))

    schema_version = document.get("schema_version")
    if schema_version != 1:
        add(
            "E_SCHEMA_VERSION",
            "<manifest>",
            "schema_version must be the integer 1",
        )

    seed = document.get("seed")
    if isinstance(seed, bool) or not isinstance(seed, int):
        add("E_SEED_INVALID", "<manifest>", "seed must be an integer")
        seed_value = 0
    else:
        seed_value = seed

    sample_rate = document.get("sample_rate")
    if isinstance(sample_rate, bool) or not isinstance(sample_rate, int):
        add("E_SAMPLE_RATE_INVALID", "<manifest>", "sample_rate must be an integer")
        sample_rate_value = 48000
    else:
        sample_rate_value = sample_rate
        if sample_rate != 48000:
            add(
                "E_SAMPLE_RATE_UNSUPPORTED",
                "<manifest>",
                "the mastering output pipeline requires 48000 Hz",
            )

    bit_depth = document.get("bit_depth")
    if bit_depth not in (16, 24):
        add(
            "E_BIT_DEPTH_INVALID",
            "<manifest>",
            "bit_depth must be 16 or 24",
        )
        bit_depth_value = 24
    else:
        bit_depth_value = int(bit_depth)

    duration = _json_number(document.get("duration_seconds"))
    if duration is None or duration <= 0:
        add(
            "E_DURATION_INVALID",
            "<manifest>",
            "duration_seconds must be a positive finite number",
        )
        duration_value = 0.0
    else:
        duration_value = duration

    target_loudness = _json_number(document.get("target_loudness_lufs"))
    if target_loudness is None or not (-70.0 <= target_loudness <= 0.0):
        add(
            "E_LOUDNESS_TARGET_INVALID",
            "<manifest>",
            "target_loudness_lufs must be finite and in [-70, 0]",
        )
        target_loudness_value = -23.0
    else:
        target_loudness_value = target_loudness

    true_peak_limit = _json_number(document.get("true_peak_limit_dbtp", -1.0))
    if true_peak_limit is None or not (-30.0 <= true_peak_limit <= 0.0):
        add(
            "E_TRUE_PEAK_LIMIT_INVALID",
            "<manifest>",
            "true_peak_limit_dbtp must be finite and in [-30, 0]",
        )
        true_peak_limit_value = -1.0
    else:
        true_peak_limit_value = true_peak_limit

    block_frames = document.get("block_frames", 24000)
    if (
        isinstance(block_frames, bool)
        or not isinstance(block_frames, int)
        or block_frames <= 0
    ):
        add(
            "E_BLOCK_SIZE_INVALID",
            "<manifest>",
            "block_frames must be a positive integer",
        )
        block_frames_value = 24000
    else:
        block_frames_value = block_frames

    raw_voices = document.get("voices")
    if not isinstance(raw_voices, list):
        add("E_VOICES_MISSING", "<manifest>", "voices must be an array")
        raw_voices = []

    voices: dict[str, VoiceSpec] = {}
    for index, raw_voice in enumerate(raw_voices):
        if not isinstance(raw_voice, dict):
            add(
                "E_VOICE_INVALID",
                f"<voice:{index}>",
                "voice entry must be an object",
            )
            continue
        raw_name = raw_voice.get("name")
        if not isinstance(raw_name, str):
            add(
                "E_VOICE_MISSING",
                f"<voice:{index}>",
                "voice name is required",
            )
            continue
        raw_key = raw_name.strip()
        canonical = VOICE_ALIASES.get(raw_key) or VOICE_ALIASES.get(raw_key.lower())
        if canonical is None or canonical not in VOICE_NAMES:
            add(
                "E_VOICE_INVALID",
                f"<voice:{raw_name}>",
                "voice name must be one of 女高/女低/男高/男低",
                voice=raw_name,
            )
            continue
        if canonical in voices:
            add(
                "E_VOICE_DUPLICATE",
                f"<voice:{canonical}>",
                "voice is declared more than once",
                voice=canonical,
            )
            continue

        ratio = _json_number(raw_voice.get("target_energy_ratio"))
        if ratio is None or ratio < 0:
            add(
                "E_TARGET_RATIO_INVALID",
                f"<voice:{canonical}>",
                "target_energy_ratio must be a non-negative finite number",
                voice=canonical,
            )
            ratio_value = 0.0
        else:
            ratio_value = ratio

        band = raw_voice.get("breakpoint_band_hz")
        if (
            not isinstance(band, list)
            or len(band) != 2
            or _json_number(band[0]) is None
            or _json_number(band[1]) is None
        ):
            add(
                "E_BAND_INVALID",
                f"<voice:{canonical}>",
                "breakpoint_band_hz must be [low_hz, high_hz]",
                voice=canonical,
            )
            band_low, band_high = 100.0, 1000.0
        else:
            band_low = float(band[0])
            band_high = float(band[1])
            if band_low < 0 or band_high > sample_rate_value / 2:
                add(
                    "E_BAND_INVALID",
                    f"<voice:{canonical}>",
                    "breakpoint band must lie between 0 Hz and Nyquist",
                    voice=canonical,
                )
            if band_low >= band_high:
                add(
                    "E_BAND_INVERTED",
                    f"<voice:{canonical}>",
                    "breakpoint band lower bound must be below upper bound",
                    voice=canonical,
                )

        pan = _json_number(raw_voice.get("pan", 0.0))
        if pan is None or not (-1.0 <= pan <= 1.0):
            add(
                "E_PAN_INVALID",
                f"<voice:{canonical}>",
                "pan must be a finite number in [-1, 1]",
                voice=canonical,
            )
            pan_value = 0.0
        else:
            pan_value = pan

        synthesis = raw_voice.get("synthesis", {})
        if not isinstance(synthesis, dict):
            add(
                "E_SYNTHESIS_INVALID",
                f"<voice:{canonical}>",
                "synthesis must be an object",
                voice=canonical,
            )
            synthesis = {}

        voices[canonical] = VoiceSpec(
            name=canonical,
            target_energy_ratio=ratio_value,
            band_low_hz=band_low,
            band_high_hz=band_high,
            pan=pan_value,
            synthesis=dict(synthesis),
        )

    for voice_name in VOICE_NAMES:
        if voice_name not in voices:
            add(
                "E_VOICE_MISSING",
                f"<voice:{voice_name}>",
                "all four voice parts are required",
                voice=voice_name,
            )

    ratio_sum = sum(voice.target_energy_ratio for voice in voices.values())
    if abs(ratio_sum - 1.0) > 1e-6:
        for voice_name in VOICE_NAMES:
            if voice_name in voices:
                add(
                    "E_RATIO_SUM",
                    f"<voice:{voice_name}>",
                    "target_energy_ratio values must sum to 1 within 1e-6",
                    voice=voice_name,
                    actual_sum=ratio_sum,
                )
        if not voices:
            add(
                "E_RATIO_SUM",
                "<manifest>",
                "target_energy_ratio values must sum to 1 within 1e-6",
                actual_sum=ratio_sum,
            )

    raw_mics = document.get("microphones")
    if not isinstance(raw_mics, list):
        add("E_MICROPHONES_MISSING", "<manifest>", "microphones must be an array")
        raw_mics = []

    microphones: list[MicSpec] = []
    seen_mics: set[str] = set()
    for index, raw_mic in enumerate(raw_mics):
        if not isinstance(raw_mic, dict):
            add(
                "E_MIC_INVALID",
                f"<microphone:{index}>",
                "microphone entry must be an object",
            )
            continue
        mic_name = raw_mic.get("name")
        if not isinstance(mic_name, str) or not mic_name.strip():
            add(
                "E_MIC_NAME_MISSING",
                f"<microphone:{index}>",
                "microphone name is required",
            )
            mic_name = f"<microphone:{index}>"
        mic_name = mic_name.strip()
        if mic_name in seen_mics:
            add(
                "E_MIC_DUPLICATE",
                mic_name,
                "microphone name is duplicated",
            )
            continue
        seen_mics.add(mic_name)

        raw_voice_name = raw_mic.get("voice")
        if not isinstance(raw_voice_name, str) or not raw_voice_name.strip():
            add(
                "E_VOICE_MISSING",
                mic_name,
                "microphone voice is required",
            )
            continue
        voice_key = raw_voice_name.strip()
        canonical_voice = VOICE_ALIASES.get(voice_key)
        if canonical_voice is None:
            canonical_voice = VOICE_ALIASES.get(voice_key.lower())
        if canonical_voice is None:
            add(
                "E_VOICE_INVALID",
                mic_name,
                "microphone voice must be one of 女高/女低/男高/男低",
                voice=voice_key,
            )
            continue
        if canonical_voice not in voices:
            add(
                "E_VOICE_MISSING",
                mic_name,
                "microphone references a voice that is not declared",
                voice=canonical_voice,
            )
            continue

        raw_path = raw_mic.get("path")
        if not isinstance(raw_path, str) or not raw_path.strip():
            add(
                "E_PATH_MISSING",
                mic_name,
                "microphone path is required",
                voice=canonical_voice,
            )
            continue
        relative_path = raw_path.strip()
        resolved_path = (manifest_path.parent / relative_path).resolve()

        channels = raw_mic.get("channels")
        if isinstance(channels, bool) or not isinstance(channels, int) or channels <= 0:
            add(
                "E_CHANNELS_INVALID",
                mic_name,
                "channels must be a positive integer",
                voice=canonical_voice,
            )
            channels_value = 1
        else:
            channels_value = channels

        position = raw_mic.get("position", {})
        if not isinstance(position, dict):
            add(
                "E_POSITION_INVALID",
                mic_name,
                "position must be an object",
                voice=canonical_voice,
            )
            position = {}
        delay_ms = _json_number(position.get("delay_ms", 0.0))
        if delay_ms is None or delay_ms < 0:
            add(
                "E_POSITION_INVALID",
                mic_name,
                "position.delay_ms must be a non-negative finite number",
                voice=canonical_voice,
            )
            delay_value = 0.0
        else:
            delay_value = delay_ms
        gain_db = _json_number(position.get("gain_db", 0.0))
        if gain_db is None:
            add(
                "E_POSITION_INVALID",
                mic_name,
                "position.gain_db must be a finite number",
                voice=canonical_voice,
            )
            gain_value = 0.0
        else:
            gain_value = gain_db

        weight = _json_number(raw_mic.get("downmix_weight", 0.5))
        if weight is None or weight < 0:
            add(
                "E_DOWNMIX_WEIGHT_INVALID",
                mic_name,
                "downmix_weight must be a non-negative finite number",
                voice=canonical_voice,
            )
            weight_value = 0.0
        else:
            weight_value = weight

        microphones.append(
            MicSpec(
                name=mic_name,
                voice=canonical_voice,
                relative_path=relative_path,
                resolved_path=resolved_path,
                channels=channels_value,
                delay_ms=delay_value,
                gain_db=gain_value,
                downmix_weight=weight_value,
            )
        )

    for voice_name in VOICE_NAMES:
        parts = [mic for mic in microphones if mic.voice == voice_name]
        if voice_name in voices and len(parts) != 2:
            add(
                "E_MIC_COUNT",
                f"<voice:{voice_name}>",
                "each voice must have exactly two microphones",
                voice=voice_name,
                actual_count=len(parts),
            )
    if len(microphones) != 8:
        add(
            "E_MIC_TOTAL",
            "<manifest>",
            "the project must contain exactly eight microphones",
            actual_count=len(microphones),
        )

    # Attach voice-level geometry errors to every microphone of that voice.
    expanded: list[Any] = []
    for current in issues:
        if current.mic.startswith("<voice:") and current.voice in voices:
            parts = [mic for mic in microphones if mic.voice == current.voice]
            if parts:
                for mic in parts:
                    expanded.append(
                        issue(
                            current.code,
                            mic.name,
                            current.message,
                            voice=current.voice,
                            **current.details,
                        )
                    )
                continue
        expanded.append(current)
    issues = expanded

    if check_files:
        for mic in microphones:
            if not mic.resolved_path.is_file():
                add(
                    "E_FILE_MISSING",
                    mic.name,
                    "declared WAV file is missing",
                    voice=mic.voice,
                    path=mic.relative_path,
                )
                continue
            try:
                info = sf.info(str(mic.resolved_path))
            except (RuntimeError, OSError, ValueError) as exc:
                add(
                    "E_FILE_UNREADABLE",
                    mic.name,
                    f"WAV cannot be opened: {type(exc).__name__}",
                    voice=mic.voice,
                    path=mic.relative_path,
                )
                continue
            if info.samplerate != sample_rate_value:
                add(
                    "E_SAMPLE_RATE_MISMATCH",
                    mic.name,
                    "actual WAV sample rate does not match the manifest",
                    voice=mic.voice,
                    expected=sample_rate_value,
                    actual=info.samplerate,
                )
            actual_depth = _subtype_bit_depth(info.subtype)
            if actual_depth != bit_depth_value:
                add(
                    "E_BIT_DEPTH_MISMATCH",
                    mic.name,
                    "actual WAV bit depth does not match the manifest",
                    voice=mic.voice,
                    expected=bit_depth_value,
                    actual=actual_depth,
                )
            if info.channels != mic.channels:
                add(
                    "E_CHANNELS_MISMATCH",
                    mic.name,
                    "actual WAV channel count does not match the manifest",
                    voice=mic.voice,
                    expected=mic.channels,
                    actual=info.channels,
                )

    if issues:
        raise BusinessError(issues, exit_code=EXIT_VALIDATION)

    return Project(
        manifest_path=manifest_path,
        manifest=document,
        voices=voices,
        microphones=sorted(microphones, key=lambda item: item.name),
        sample_rate=sample_rate_value,
        bit_depth=bit_depth_value,
        seed=seed_value,
        duration_seconds=duration_value,
        target_loudness_lufs=target_loudness_value,
        true_peak_limit_dbtp=true_peak_limit_value,
        block_frames=block_frames_value,
    )


def input_digest(project: Project) -> str:
    digest = hashlib.sha256()
    for mic in sorted(project.microphones, key=lambda item: item.name):
        digest.update(mic.name.encode("utf-8"))
        digest.update(b"\0")
        with open(mic.resolved_path, "rb") as handle:
            while True:
                chunk = handle.read(1024 * 1024)
                if not chunk:
                    break
                digest.update(chunk)
        digest.update(b"\0")
    return digest.hexdigest()

