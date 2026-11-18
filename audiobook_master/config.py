"""Configuration parsing, schema checks and package manifest planning."""

from __future__ import annotations

import json
import math
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import BusinessError
from .util import sha256_file, sha256_json
from .wavio import WavAudio, read_wav


@dataclass(slots=True, frozen=True)
class ChapterRule:
    id: str
    expected_duration_seconds: float
    expected_frames: int
    field_path: str


@dataclass(slots=True, frozen=True)
class RuleConfig:
    sample_rate: int
    bit_depth: int
    channels: int | None
    target_loudness_lufs: float
    relative_gate_lu: float
    true_peak_dbtp: float
    max_noise_floor_rise_db: float
    boundary_tolerance_db: float
    block_frames: int
    chapters: tuple[ChapterRule, ...]
    raw: dict[str, Any]
    content_hash: str


@dataclass(slots=True, frozen=True)
class PackagePlan:
    chapter_id: str
    index: int
    path: Path
    report_file: str
    channels: int
    batch: str
    declared_sample_rate: int | None
    expected_duration_seconds: float
    expected_frames: int
    expected_frames_known_rate: bool
    field_prefix: str
    declared_bit_depth: int | None


@dataclass(slots=True, frozen=True)
class ChapterPlan:
    rule: ChapterRule
    packages: tuple[PackagePlan, ...]
    declared_sample_rate: int | None


@dataclass(slots=True, frozen=True)
class ManifestPlan:
    path: Path
    raw: dict[str, Any]
    chapters: tuple[ChapterPlan, ...]


@dataclass(slots=True)
class LoadedPackage:
    plan: PackagePlan
    audio: WavAudio
    sha256: str


@dataclass(slots=True)
class LoadedChapter:
    plan: ChapterPlan
    packages: tuple[LoadedPackage, ...]
    source_samples: np.ndarray
    source_sample_rate: int
    master_samples: np.ndarray


@dataclass(slots=True)
class ValidatedBook:
    rules: RuleConfig
    manifest: ManifestPlan
    chapters: tuple[LoadedChapter, ...]
    master_channels: int
    input_digest: str
    input_summary: tuple[dict[str, Any], ...]


def read_json_object(path: Path, label: str) -> dict[str, Any]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise BusinessError(
            "E_CONFIG_INVALID", f"cannot read {label} JSON: {exc}", label
        ) from exc
    try:
        value = json.loads(text, object_pairs_hook=_reject_duplicate_keys)
    except (json.JSONDecodeError, ValueError) as exc:
        raise BusinessError(
            "E_CONFIG_INVALID", f"invalid {label} JSON: {exc}", label
        ) from exc
    if not isinstance(value, dict):
        raise BusinessError(
            "E_CONFIG_INVALID", f"{label} must be a JSON object", label
        )
    return value


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate key {key!r}")
        result[key] = value
    return result


def load_rule_config(path: Path) -> RuleConfig:
    raw = read_json_object(path, "rules")
    sample_rate = _int_field(raw, "sample_rate", "rules.sample_rate", minimum=8000)
    bit_depth = _int_field(raw, "bit_depth", "rules.bit_depth")
    if bit_depth not in (16, 24, 32):
        raise BusinessError(
            "E_CONFIG_INVALID",
            "bit_depth must be one of 16, 24 or 32",
            "rules.bit_depth",
        )

    channels_value = _alias(raw, ("channels", "output_channels"))
    channels: int | None = None
    if channels_value is not None:
        channels = _positive_int(channels_value, "rules.channels")
        if channels > 8:
            raise BusinessError(
                "E_CONFIG_INVALID",
                "channels must be between 1 and 8",
                "rules.channels",
            )

    target = _float_alias(
        raw,
        ("target_loudness_lufs", "target_lufs", "target_loudness"),
        "rules.target_loudness_lufs",
    )
    if not (-40.0 <= target <= -5.0):
        raise BusinessError(
            "E_CONFIG_INVALID",
            f"target loudness {target:g} LUFS is outside the reasonable "
            "interval [-40, -5]",
            "rules.target_loudness_lufs",
        )

    relative_gate = _float_alias(
        raw,
        ("relative_gate_lu", "relative_gate_lufs", "gate_lu"),
        "rules.relative_gate_lu",
        default=-10.0,
    )
    if not (0.0 < abs(relative_gate) <= 30.0):
        raise BusinessError(
            "E_CONFIG_INVALID",
            "relative gate magnitude must be in (0, 30] LU",
            "rules.relative_gate_lu",
        )

    true_peak = _float_alias(
        raw,
        ("true_peak_dbtp", "max_true_peak_dbtp", "true_peak_max_dbtp"),
        "rules.true_peak_dbtp",
    )
    if true_peak > 0.0:
        raise BusinessError(
            "E_CONFIG_INVALID",
            "true peak ceiling must not be above 0 dBTP",
            "rules.true_peak_dbtp",
        )
    noise_limit = _float_alias(
        raw,
        (
            "max_noise_floor_rise_db",
            "noise_floor_rise_limit_db",
            "max_noise_rise_db",
        ),
        "rules.max_noise_floor_rise_db",
        default=6.0,
    )
    if not (0.0 <= noise_limit <= 60.0):
        raise BusinessError(
            "E_CONFIG_INVALID",
            "maximum noise-floor rise must be between 0 and 60 dB",
            "rules.max_noise_floor_rise_db",
        )

    boundary_tolerance = _float_alias(
        raw,
        ("boundary_tolerance_db", "max_boundary_level_change_db"),
        "rules.boundary_tolerance_db",
        default=0.5,
    )
    if not (0.0 < boundary_tolerance <= 12.0):
        raise BusinessError(
            "E_CONFIG_INVALID",
            "boundary tolerance must be in (0, 12] dB",
            "rules.boundary_tolerance_db",
        )

    block_frames = _int_alias(
        raw,
        ("block_frames", "render_block_frames"),
        "rules.block_frames",
        default=16384,
    )
    if block_frames < 256:
        raise BusinessError(
            "E_CONFIG_INVALID",
            "render block must contain at least 256 frames",
            "rules.block_frames",
        )

    raw_chapters = raw.get("chapters")
    if not isinstance(raw_chapters, (list, dict)) or not raw_chapters:
        raise BusinessError(
            "E_CONFIG_INVALID",
            "rules must declare a non-empty chapters list or object",
            "rules.chapters",
        )
    chapter_items: list[tuple[str | None, dict[str, Any], str]]
    if isinstance(raw_chapters, list):
        chapter_items = []
        for index, item in enumerate(raw_chapters):
            prefix = f"rules.chapters[{index}]"
            if not isinstance(item, dict):
                raise BusinessError(
                    "E_CONFIG_INVALID", "chapter rule must be an object", prefix
                )
            chapter_items.append((None, item, prefix))
    else:
        chapter_items = []
        for chapter_id, item in raw_chapters.items():
            prefix = f"rules.chapters.{chapter_id}"
            if not isinstance(item, dict):
                raise BusinessError(
                    "E_CONFIG_INVALID", "chapter rule must be an object", prefix
                )
            chapter_items.append((chapter_id, item, prefix))

    chapters: list[ChapterRule] = []
    seen_ids: set[str] = set()
    for supplied_id, item, prefix in chapter_items:
        chapter_id = supplied_id or _string_alias(
            item, ("id", "chapter_id"), f"{prefix}.id"
        )
        if chapter_id in seen_ids:
            raise BusinessError(
                "E_CONFIG_INVALID",
                f"duplicate chapter id {chapter_id!r}",
                f"{prefix}.id",
            )
        seen_ids.add(chapter_id)

        duration_value = _alias(
            item,
            (
                "expected_duration_seconds",
                "duration_seconds",
                "expected_duration",
                "duration",
            ),
        )
        samples_value = _alias(item, ("expected_samples", "samples", "frames"))
        if duration_value is None and samples_value is None:
            raise BusinessError(
                "E_CONFIG_INVALID",
                "chapter must declare expected_duration_seconds or expected_samples",
                f"{prefix}.expected_duration_seconds",
            )
        if duration_value is not None:
            duration = _positive_float(
                duration_value, f"{prefix}.expected_duration_seconds"
            )
            expected_frames = int(round(duration * sample_rate))
        else:
            expected_frames = _positive_int(
                samples_value, f"{prefix}.expected_samples"
            )
            duration = expected_frames / sample_rate
        if expected_frames < 1:
            raise BusinessError(
                "E_CONFIG_INVALID",
                "expected chapter duration rounds to zero frames",
                f"{prefix}.expected_duration_seconds",
            )
        chapters.append(
            ChapterRule(
                id=chapter_id,
                expected_duration_seconds=duration,
                expected_frames=expected_frames,
                field_path=prefix,
            )
        )

    canonical_rules = {
        "sample_rate": sample_rate,
        "bit_depth": bit_depth,
        "channels": channels,
        "target_loudness_lufs": target,
        "relative_gate_lu": relative_gate,
        "true_peak_dbtp": true_peak,
        "max_noise_floor_rise_db": noise_limit,
        "boundary_tolerance_db": boundary_tolerance,
        "block_frames": block_frames,
        "chapters": [
            {
                "id": chapter.id,
                "expected_duration_seconds": chapter.expected_duration_seconds,
                "expected_frames": chapter.expected_frames,
            }
            for chapter in chapters
        ],
    }
    return RuleConfig(
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        channels=channels,
        target_loudness_lufs=target,
        relative_gate_lu=relative_gate,
        true_peak_dbtp=true_peak,
        max_noise_floor_rise_db=noise_limit,
        boundary_tolerance_db=boundary_tolerance,
        block_frames=block_frames,
        chapters=tuple(chapters),
        raw=raw,
        content_hash=sha256_json(canonical_rules),
    )


def load_manifest_plan(path: Path, rules: RuleConfig) -> ManifestPlan:
    raw = read_json_object(path, "manifest")
    raw_chapters = raw.get("chapters")
    if not isinstance(raw_chapters, (list, dict)) or not raw_chapters:
        raise BusinessError(
            "E_CONFIG_INVALID",
            "manifest must declare a non-empty chapters list or object",
            "manifest.chapters",
        )

    manifest_items: dict[str, tuple[dict[str, Any], str]] = {}
    if isinstance(raw_chapters, list):
        for index, item in enumerate(raw_chapters):
            prefix = f"manifest.chapters[{index}]"
            if not isinstance(item, dict):
                raise BusinessError(
                    "E_CONFIG_INVALID", "manifest chapter must be an object", prefix
                )
            chapter_id = _string_alias(
                item, ("id", "chapter_id"), f"{prefix}.id"
            )
            if chapter_id in manifest_items:
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    f"duplicate manifest chapter id {chapter_id!r}",
                    f"{prefix}.id",
                )
            manifest_items[chapter_id] = (item, prefix)
    else:
        for chapter_id, item in raw_chapters.items():
            prefix = f"manifest.chapters.{chapter_id}"
            if not isinstance(item, dict):
                raise BusinessError(
                    "E_CONFIG_INVALID", "manifest chapter must be an object", prefix
                )
            manifest_items[chapter_id] = (item, prefix)

    extra_ids = sorted(set(manifest_items) - {chapter.id for chapter in rules.chapters})
    if extra_ids:
        item, prefix = manifest_items[extra_ids[0]]
        raise BusinessError(
            "E_CONFIG_INVALID",
            f"manifest chapter {extra_ids[0]!r} is not declared in rules",
            f"{prefix}.id",
        )

    base_dir = path.parent
    chapters: list[ChapterPlan] = []
    for rule in rules.chapters:
        if rule.id not in manifest_items:
            raise BusinessError(
                "E_CONFIG_INVALID",
                f"manifest is missing chapter {rule.id!r}",
                "manifest.chapters",
            )
        item, prefix = manifest_items[rule.id]
        raw_packages = item.get("packages")
        if raw_packages is None:
            if "file" in item or "path" in item or "wav" in item:
                raw_packages = [item]
            else:
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "chapter must contain a packages array",
                    f"{prefix}.packages",
                )
        if not isinstance(raw_packages, list) or not raw_packages:
            raise BusinessError(
                "E_CONFIG_INVALID",
                "packages must be a non-empty array",
                f"{prefix}.packages",
            )
        chapter_rate_value = _alias(item, ("sample_rate", "sample_rate_hz"))
        chapter_rate: int | None = None
        if chapter_rate_value is not None:
            chapter_rate = _positive_int(
                chapter_rate_value, f"{prefix}.sample_rate"
            )

        package_data: list[dict[str, Any]] = []
        for index, package_item in enumerate(raw_packages):
            package_prefix = f"{prefix}.packages[{index}]"
            if not isinstance(package_item, dict):
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "package entry must be an object",
                    package_prefix,
                )
            package_data.append((package_item, package_prefix))

        explicit_rate: int | None = chapter_rate
        for package_item, package_prefix in package_data:
            rate_value = _alias(
                package_item, ("sample_rate", "sample_rate_hz")
            )
            if rate_value is not None:
                rate = _positive_int(
                    rate_value, f"{package_prefix}.sample_rate"
                )
                if explicit_rate is not None and rate != explicit_rate:
                    raise BusinessError(
                        "E_CONFIG_INVALID",
                        "packages in the same chapter declare different "
                        f"sample rates ({explicit_rate} vs {rate})",
                        f"{package_prefix}.sample_rate",
                    )
                explicit_rate = rate

        durations = [
            _package_duration(
                package_item,
                package_prefix,
                explicit_rate or rules.sample_rate,
            )
            for package_item, package_prefix in package_data
        ]
        if any(value is not None for value in durations):
            missing = [index for index, value in enumerate(durations) if value is None]
            known_total = sum(value for value in durations if value is not None)
            if known_total > rule.expected_duration_seconds + 1e-9:
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "package durations exceed the chapter expectation",
                    f"{prefix}.packages",
                )
            if missing:
                remaining = rule.expected_duration_seconds - known_total
                share = remaining / len(missing)
                for index in missing:
                    durations[index] = share
        else:
            share = rule.expected_duration_seconds / len(package_data)
            durations = [share for _ in package_data]

        total_duration = sum(value for value in durations if value is not None)
        tolerance = 0.5 / rules.sample_rate
        if abs(total_duration - rule.expected_duration_seconds) > tolerance:
            raise BusinessError(
                "E_CONFIG_INVALID",
                "package durations do not add up to the expected chapter duration",
                f"{prefix}.packages",
            )

        packages: list[PackagePlan] = []
        for index, ((package_item, package_prefix), duration) in enumerate(
            zip(package_data, durations)
        ):
            assert duration is not None
            file_value = _string_alias(
                package_item,
                ("file", "path", "wav"),
                f"{package_prefix}.file",
            )
            target_path = Path(file_value)
            if not target_path.is_absolute():
                target_path = base_dir / target_path
            chapters_dir = base_dir
            try:
                report_file = Path(
                    os.path.relpath(target_path, chapters_dir)
                ).as_posix()
            except ValueError:
                report_file = target_path.name
            channels = _positive_int(
                _alias(package_item, ("channels", "channel_count")),
                f"{package_prefix}.channels",
            )
            batch = _string_alias(
                package_item,
                ("batch", "batch_id", "recording_batch"),
                f"{package_prefix}.batch",
            )
            rate_value = _alias(
                package_item, ("sample_rate", "sample_rate_hz")
            )
            package_rate: int | None
            if rate_value is not None:
                package_rate = _positive_int(
                    rate_value, f"{package_prefix}.sample_rate"
                )
            else:
                package_rate = chapter_rate or explicit_rate
            effective_rate = package_rate or rules.sample_rate
            expected_frames = int(round(duration * effective_rate))
            if expected_frames < 1:
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "package duration rounds to zero frames",
                    f"{package_prefix}.expected_duration_seconds",
                )
            bit_depth_value = _alias(
                package_item, ("bit_depth", "bits_per_sample")
            )
            declared_bit_depth: int | None = None
            if bit_depth_value is not None:
                declared_bit_depth = _positive_int(
                    bit_depth_value, f"{package_prefix}.bit_depth"
                )
                if declared_bit_depth not in (16, 24, 32):
                    raise BusinessError(
                        "E_CONFIG_INVALID",
                        "input bit depth must be 16, 24 or 32",
                        f"{package_prefix}.bit_depth",
                    )
            packages.append(
                PackagePlan(
                    chapter_id=rule.id,
                    index=index,
                    path=target_path,
                    report_file=report_file,
                    channels=channels,
                    batch=batch,
                    declared_sample_rate=package_rate,
                    expected_duration_seconds=duration,
                    expected_frames=expected_frames,
                    expected_frames_known_rate=package_rate is not None,
                    field_prefix=package_prefix,
                    declared_bit_depth=declared_bit_depth,
                )
            )

        if any(package.channels != packages[0].channels for package in packages):
            for index, package in enumerate(packages[1:], start=1):
                if package.channels != packages[0].channels:
                    raise BusinessError(
                        "E_CONFIG_INVALID",
                        "packages in the same chapter declare different channel counts",
                        f"{packages[index].field_prefix}.channels",
                    )

        chapters.append(
            ChapterPlan(
                rule=rule,
                packages=tuple(packages),
                declared_sample_rate=explicit_rate,
            )
        )

    return ManifestPlan(path=path, raw=raw, chapters=tuple(chapters))


def _package_duration(
    item: dict[str, Any], prefix: str, fallback_rate: int
) -> float | None:
    duration_value = _alias(
        item,
        (
            "expected_duration_seconds",
            "duration_seconds",
            "expected_duration",
            "duration",
        ),
    )
    if duration_value is not None:
        return _positive_float(
            duration_value, f"{prefix}.expected_duration_seconds"
        )
    samples_value = _alias(
        item, ("expected_samples", "samples", "frames")
    )
    if samples_value is not None:
        frames = _positive_int(samples_value, f"{prefix}.expected_samples")
        rate_value = _alias(item, ("sample_rate", "sample_rate_hz"))
        rate = (
            _positive_int(rate_value, f"{prefix}.sample_rate")
            if rate_value is not None
            else fallback_rate
        )
        return frames / rate
    return None


def load_and_validate(rules_path: str | Path, manifest_path: str | Path) -> ValidatedBook:
    rules = load_rule_config(Path(rules_path))
    manifest = load_manifest_plan(Path(manifest_path), rules)

    loaded_chapters: list[LoadedChapter] = []
    summary: list[dict[str, Any]] = []
    all_channel_counts: set[int] = set()
    for chapter in manifest.chapters:
        loaded_packages: list[LoadedPackage] = []
        actual_rates: set[int] = set()
        channel_count: int | None = None
        pieces: list[np.ndarray] = []
        for package in chapter.packages:
            audio = read_wav(package.path, field_path=f"{package.field_prefix}.file")
            actual_rates.add(audio.info.sample_rate)
            if package.declared_sample_rate is not None:
                if audio.info.sample_rate != package.declared_sample_rate:
                    raise BusinessError(
                        "E_CONFIG_INVALID",
                        "WAV sample rate does not match the manifest declaration",
                        f"{package.field_prefix}.sample_rate",
                    )
            if audio.info.channels != package.channels:
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "WAV channel count does not match the manifest declaration",
                    f"{package.field_prefix}.channels",
                )
            if (
                package.declared_bit_depth is not None
                and audio.info.bit_depth != package.declared_bit_depth
            ):
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "WAV bit depth does not match the manifest declaration",
                    f"{package.field_prefix}.bit_depth",
                )
            if channel_count is None:
                channel_count = audio.info.channels
            elif channel_count != audio.info.channels:
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "packages in the same chapter have different channel counts",
                    f"{package.field_prefix}.channels",
                )
            expected_duration = audio.info.frames / audio.info.sample_rate
            duration_tolerance = max(
                0.5 / audio.info.sample_rate,
                0.5 / rules.sample_rate,
            )
            if (
                abs(expected_duration - package.expected_duration_seconds)
                > duration_tolerance
            ):
                raise BusinessError(
                    "E_CONFIG_INVALID",
                    "package frame count does not match the declared duration",
                    f"{package.field_prefix}.expected_duration_seconds",
                    details={
                        "actual_seconds": expected_duration,
                        "expected_seconds": package.expected_duration_seconds,
                    },
                )
            digest = sha256_file(package.path)
            loaded_packages.append(
                LoadedPackage(plan=package, audio=audio, sha256=digest)
            )
            pieces.append(audio.samples)
            summary.append(
                {
                    "chapter": chapter.rule.id,
                    "file": package.report_file,
                    "batch": package.batch,
                    "sha256": digest,
                    "sample_rate": audio.info.sample_rate,
                    "channels": audio.info.channels,
                    "bit_depth": audio.info.bit_depth,
                    "frames": audio.info.frames,
                    "effective_samples": audio.info.frames,
                    "duration_seconds": audio.info.frames / audio.info.sample_rate,
                    "peak_dbfs": _sample_peak_dbfs(audio.samples),
                }
            )
        if len(actual_rates) > 1:
            raise BusinessError(
                "E_CONFIG_INVALID",
                "packages in the same chapter have different WAV sample rates",
                f"{chapter.packages[-1].field_prefix}.sample_rate",
            )
        source_rate = next(iter(actual_rates))
        source_samples = np.concatenate(pieces, axis=0)
        chapter_master_frames = chapter.rule.expected_frames
        master_samples = _convert_channels(
            _resample(source_samples, source_rate, rules.sample_rate),
            rules.channels,
        )
        if master_samples.shape[0] != chapter_master_frames:
            master_samples = _fit_frames(master_samples, chapter_master_frames)
        all_channel_counts.add(master_samples.shape[1])
        loaded_chapters.append(
            LoadedChapter(
                plan=chapter,
                packages=tuple(loaded_packages),
                source_samples=source_samples,
                source_sample_rate=source_rate,
                master_samples=master_samples,
            )
        )

    if rules.channels is not None:
        master_channels = rules.channels
    elif len(all_channel_counts) == 1:
        master_channels = next(iter(all_channel_counts))
    else:
        raise BusinessError(
            "E_CONFIG_INVALID",
            "chapters have different channel counts; declare rules.channels "
            "to request a deterministic conversion",
            "manifest.chapters",
        )

    digest = _input_digest(rules, manifest, summary)
    return ValidatedBook(
        rules=rules,
        manifest=manifest,
        chapters=tuple(loaded_chapters),
        master_channels=master_channels,
        input_digest=digest,
        input_summary=tuple(summary),
    )


def _resample(samples: np.ndarray, source_rate: int, target_rate: int) -> np.ndarray:
    if source_rate == target_rate:
        return np.ascontiguousarray(samples, dtype=np.float64)
    source_frames = samples.shape[0]
    target_frames = int(round(source_frames * target_rate / source_rate))
    if target_frames < 1:
        return np.zeros((0, samples.shape[1]), dtype=np.float64)
    source_positions = np.arange(source_frames, dtype=np.float64)
    target_positions = np.arange(target_frames, dtype=np.float64) * (
        source_rate / target_rate
    )
    output = np.empty((target_frames, samples.shape[1]), dtype=np.float64)
    for channel in range(samples.shape[1]):
        output[:, channel] = np.interp(
            target_positions, source_positions, samples[:, channel]
        )
    return output


def _convert_channels(samples: np.ndarray, target_channels: int | None) -> np.ndarray:
    if target_channels is None or samples.shape[1] == target_channels:
        return np.ascontiguousarray(samples, dtype=np.float64)
    if samples.shape[1] == 1:
        return np.repeat(samples, target_channels, axis=1)
    if target_channels == 1:
        return np.mean(samples, axis=1, keepdims=True)
    if target_channels < samples.shape[1]:
        return np.ascontiguousarray(samples[:, :target_channels], dtype=np.float64)
    repeats = int(math.ceil(target_channels / samples.shape[1]))
    tiled = np.tile(samples, (1, repeats))
    return np.ascontiguousarray(tiled[:, :target_channels], dtype=np.float64)


def _fit_frames(samples: np.ndarray, frames: int) -> np.ndarray:
    if samples.shape[0] >= frames:
        return np.ascontiguousarray(samples[:frames], dtype=np.float64)
    missing = frames - samples.shape[0]
    padding = np.zeros((missing, samples.shape[1]), dtype=np.float64)
    return np.concatenate([samples, padding], axis=0)


def _input_digest(
    rules: RuleConfig,
    manifest: ManifestPlan,
    summary: list[dict[str, Any]],
) -> str:
    normalized_manifest = {
        "chapters": [
            {
                "id": chapter.rule.id,
                "packages": [
                    {
                        "file": package.report_file,
                        "batch": package.batch,
                        "channels": package.channels,
                        "sample_rate": package.declared_sample_rate,
                        "sha256": None,
                        "frames": package.expected_frames,
                    }
                    for package in chapter.packages
                ],
            }
            for chapter in manifest.chapters
        ],
        "input_summary": summary,
        "master_channels": rules.channels,
    }
    return sha256_json(normalized_manifest)


def _sample_peak_dbfs(samples: np.ndarray) -> float:
    peak = float(np.max(np.abs(samples))) if samples.size else 0.0
    if peak <= 0.0:
        return -120.0
    return max(-120.0, 20.0 * math.log10(peak))


def _alias(mapping: dict[str, Any], names: tuple[str, ...]) -> Any:
    for name in names:
        if name in mapping:
            return mapping[name]
    return None


def _float_alias(
    mapping: dict[str, Any],
    names: tuple[str, ...],
    path: str,
    *,
    default: float | None = None,
) -> float:
    value = _alias(mapping, names)
    if value is None:
        if default is None:
            raise BusinessError("E_CONFIG_INVALID", "required field is missing", path)
        return default
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise BusinessError("E_CONFIG_INVALID", "field must be a number", path)
    number = float(value)
    if not math.isfinite(number):
        raise BusinessError("E_CONFIG_INVALID", "field must be finite", path)
    return number


def _int_alias(
    mapping: dict[str, Any],
    names: tuple[str, ...],
    path: str,
    *,
    default: int | None = None,
) -> int:
    value = _alias(mapping, names)
    if value is None:
        if default is None:
            raise BusinessError("E_CONFIG_INVALID", "required field is missing", path)
        return default
    return _integer(value, path)


def _int_field(
    mapping: dict[str, Any], name: str, path: str, *, minimum: int | None = None
) -> int:
    if name not in mapping:
        raise BusinessError("E_CONFIG_INVALID", "required field is missing", path)
    value = _integer(mapping[name], path)
    if minimum is not None and value < minimum:
        raise BusinessError(
            "E_CONFIG_INVALID", f"field must be at least {minimum}", path
        )
    return value


def _integer(value: Any, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise BusinessError("E_CONFIG_INVALID", "field must be an integer", path)
    return int(value)


def _positive_int(value: Any, path: str) -> int:
    number = _integer(value, path)
    if number < 1:
        raise BusinessError("E_CONFIG_INVALID", "field must be positive", path)
    return number


def _positive_float(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise BusinessError("E_CONFIG_INVALID", "field must be a number", path)
    number = float(value)
    if not math.isfinite(number) or number <= 0.0:
        raise BusinessError("E_CONFIG_INVALID", "field must be positive", path)
    return number


def _string_alias(
    mapping: dict[str, Any], names: tuple[str, ...], path: str
) -> str:
    value = _alias(mapping, names)
    if value is None:
        raise BusinessError("E_CONFIG_INVALID", "required field is missing", path)
    if not isinstance(value, str) or not value.strip():
        raise BusinessError(
            "E_CONFIG_INVALID", "field must be a non-empty string", path
        )
    return value
