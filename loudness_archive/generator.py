from __future__ import annotations

import shutil
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .archive import ArchiveService
from .config import Config
from .loudness import LoudnessAnalyzer, default_downmix_weights, round_db
from .metadata import build_v3_document, parse_metadata
from .publish import FileVersionPublisher
from .review import review_versions
from .trace import TraceLogger
from .util import (
    atomic_write_pretty_json,
    canonical_json_bytes,
    read_json,
    sha256_bytes,
)
from .wav import (
    DEFAULT_LAYOUTS,
    PCM,
    PCM_SUBFORMAT_GUID,
    WAVE_FORMAT_EXTENSIBLE,
    read_wav,
)

SEGMENT_SAMPLES = 24000
SEGMENT_COUNT = 5
TRUE_PEAK_LIMIT = -1.0


@dataclass(frozen=True)
class SampleSpec:
    suffix: str
    layout: str
    bits: int
    variant: int


SPECS = (
    SampleSpec("mono-16", "MONO", 16, 0),
    SampleSpec("surround-24", "5.1", 24, 1),
    SampleSpec("mono-24", "MONO", 24, 2),
    SampleSpec("surround-16", "5.1", 16, 3),
)


class SimulatedCrash(RuntimeError):
    pass


def _quantize(audio: np.ndarray, bits: int) -> bytes:
    if bits == 16:
        values = np.rint(np.clip(audio, -0.999969, 0.999969) * 32768.0)
        integers = np.clip(values, -32768, 32767).astype("<i2")
        return integers.tobytes(order="C")
    if bits == 24:
        values = np.rint(np.clip(audio, -0.999999, 0.999999) * 8388608.0)
        integers = np.clip(values, -8388608, 8388607).astype(np.int32).reshape(-1)
        unsigned = integers & 0xFFFFFF
        packed = np.empty((unsigned.size, 3), dtype=np.uint8)
        packed[:, 0] = (unsigned & 0xFF).astype(np.uint8)
        packed[:, 1] = ((unsigned >> 8) & 0xFF).astype(np.uint8)
        packed[:, 2] = ((unsigned >> 16) & 0xFF).astype(np.uint8)
        return packed.tobytes(order="C")
    raise ValueError("only 16-bit and 24-bit PCM are supported")


def write_pcm_wav(
    path: str | Path,
    audio: np.ndarray,
    *,
    sample_rate: int,
    bits: int,
    layout: str,
    force_extensible: bool = False,
) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    channels = audio.shape[1]
    channel_mask, _ = DEFAULT_LAYOUTS[layout]
    if channels != len(DEFAULT_LAYOUTS[layout][1]):
        raise ValueError("audio channel count does not match layout")
    data = _quantize(audio, bits)
    block_align = channels * (bits // 8)
    byte_rate = sample_rate * block_align
    extensible = force_extensible or layout != "MONO"
    if extensible:
        fmt = struct.pack(
            "<HHIIHHHHI16s",
            WAVE_FORMAT_EXTENSIBLE,
            channels,
            sample_rate,
            byte_rate,
            block_align,
            bits,
            22,
            bits,
            channel_mask,
            PCM_SUBFORMAT_GUID,
        )
    else:
        fmt = struct.pack(
            "<HHIIHH",
            PCM,
            channels,
            sample_rate,
            byte_rate,
            block_align,
            bits,
        )
    fmt_chunk = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    data_chunk = b"data" + struct.pack("<I", len(data)) + data
    if len(data) & 1:
        data_chunk += b"\x00"
    riff_payload = b"WAVE" + fmt_chunk + data_chunk
    blob = b"RIFF" + struct.pack("<I", len(riff_payload)) + riff_payload
    target.write_bytes(blob)


def _build_audio(seed: int, layout: str, variant: int, bits: int) -> np.ndarray:
    rng = np.random.default_rng(seed * 1009 + variant)
    frames = SEGMENT_SAMPLES * SEGMENT_COUNT
    time = np.arange(frames, dtype=np.float64) / 48000.0
    amplitudes = np.array([0.10, 0.48, 0.98, 0.22, 0.98], dtype=np.float64)
    amplitudes *= 0.98 + 0.02 * rng.random(SEGMENT_COUNT)
    segment_envelope = np.repeat(amplitudes, SEGMENT_SAMPLES)
    channels = 1 if layout == "MONO" else 6
    gains = (
        np.array([1.0], dtype=np.float64)
        if channels == 1
        else np.array([1.0, 0.96, 1.02, 0.03, 0.72, 0.72], dtype=np.float64)
    )
    audio = np.empty((frames, channels), dtype=np.float64)
    for channel in range(channels):
        phase = float(rng.random() * 0.15)
        frequency = 430.0 + variant * 11.0 + channel * 19.0
        tone = np.sin(2.0 * np.pi * frequency * time + phase)
        overtone = 0.32 * np.sin(
            2.0 * np.pi * (11100.0 + channel * 37.0) * time + phase * 0.5
        )
        audio[:, channel] = segment_envelope * gains[channel] * (tone + overtone) / 1.32
    transient_start = 2 * SEGMENT_SAMPLES + 317
    transient = np.zeros((29, channels), dtype=np.float64)
    envelope = np.exp(-np.arange(29) / 3.5)
    for channel in range(channels):
        sign = -1.0 if channel % 2 else 1.0
        transient[:, channel] = (
            sign * 0.72 * envelope * gains[channel]
        )
    audio[transient_start : transient_start + 29] += transient
    audio = audio - np.mean(audio, axis=0, keepdims=True)
    peak = float(np.max(np.abs(audio)))
    if peak > 0.995:
        audio *= 0.995 / peak
    if bits == 16:
        audio = np.rint(audio * 32768.0) / 32768.0
    return audio


def _v2_metadata(
    wav: Any,
    config: Config,
    analyzer: LoudnessAnalyzer,
    *,
    flip_flag: bool,
) -> dict[str, Any]:
    count = 4
    boundaries = []
    for index in range(count):
        start = index * wav.frames // count
        end = (index + 1) * wav.frames // count
        boundaries.append((start, end))
    analyses = analyzer.analyze_boundaries(
        boundaries,
        TRUE_PEAK_LIMIT,
        "derived_equal_count:v2",
    )
    segments = []
    for analysis in analyses:
        flag = analysis.over_limit
        if flip_flag and analysis.index == 1:
            flag = not flag
        segments.append(
            {
                "index": analysis.index,
                "short_term_lufs": round_db(analysis.short_term_lufs),
                "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
                "over_limit": flag,
            }
        )
    integrated = analyzer.integrated_lufs()
    return {
        "version": 2,
        "asset_id": wav.file_sha256,
        "sample_rate": wav.sample_rate,
        "duration_samples": wav.frames,
        "integrated_lufs": round_db(integrated),
        "target_offset_lu": round_db(integrated - config.target_lufs),
        "segment_count": count,
        "true_peak_limit_dbtp": TRUE_PEAK_LIMIT,
        "segments": segments,
    }


def _v1_metadata(wav: Any, config: Config, integrated: float) -> dict[str, Any]:
    return {
        "version": 1,
        "asset_id": wav.file_sha256,
        "sample_rate": wav.sample_rate,
        "duration_samples": wav.frames,
        "integrated_lufs": round_db(integrated),
        "target_offset_lu": round_db(integrated - config.target_lufs),
    }


def _v3_metadata(
    wav: Any,
    config: Config,
    analyzer: LoudnessAnalyzer,
) -> dict[str, Any]:
    boundaries = tuple(
        (index * SEGMENT_SAMPLES, (index + 1) * SEGMENT_SAMPLES)
        for index in range(SEGMENT_COUNT)
    )
    analyses = analyzer.analyze_boundaries(
        boundaries,
        TRUE_PEAK_LIMIT,
        "metadata.explicit_boundaries",
    )
    segments = [
        {
            "index": analysis.index,
            "start_sample": analysis.start_sample,
            "end_sample": analysis.end_sample,
            "short_term_lufs": round_db(analysis.short_term_lufs),
            "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
            "true_peak_limit_dbtp": TRUE_PEAK_LIMIT,
            "over_limit": analysis.over_limit,
            "excess_db": round_db(analysis.excess_db),
        }
        for analysis in analyses
    ]
    integrated = analyzer.integrated_lufs()
    weights = default_downmix_weights(wav.channel_layout, wav.channel_names)
    return build_v3_document(
        asset_id=wav.file_sha256,
        sample_rate=wav.sample_rate,
        duration_samples=wav.frames,
        integrated_lufs=integrated,
        target_offset_lu=integrated - config.target_lufs,
        channel_layout=wav.channel_layout,
        channel_mask=wav.channel_mask,
        downmix_weights=weights,
        true_peak_limit_dbtp=TRUE_PEAK_LIMIT,
        quantization_tolerance_db=config.quantization_tolerance_db,
        segments=segments,
    )


def _metadata_hash(payload: dict[str, Any]) -> str:
    return sha256_bytes(canonical_json_bytes(payload))


def _asset_id_for(wav: Any) -> str:
    return "wav-" + str(wav.file_sha256).split(":", 1)[-1]


def _trace_baseline(
    trace: TraceLogger,
    asset_id: str,
    version: int,
    segments: list[dict[str, Any]],
    *,
    segment_index_override: int | None = None,
) -> None:
    if not segments:
        trace.record(
            "baseline",
            asset_id=asset_id,
            metadata_version=version,
            segment_index=segment_index_override if segment_index_override is not None else -1,
            over_limit=None,
            hash=_metadata_hash({"version": version, "asset_id": asset_id}),
        )
        return
    for row in segments:
        trace.record(
            "baseline",
            asset_id=asset_id,
            metadata_version=version,
            segment_index=int(row["index"]),
            start_sample=row.get("start_sample"),
            end_sample=row.get("end_sample"),
            short_term_lufs=row.get("short_term_lufs"),
            true_peak_dbtp=row.get("true_peak_dbtp"),
            over_limit=row.get("over_limit"),
            hash=_metadata_hash(row),
        )


def generate_bundle(
    out_dir: str | Path,
    config: Config,
    *,
    seed: int = 20261122,
) -> dict[str, Any]:
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    samples_dir = output / "samples"
    metadata_dir = output / "metadata"
    demo_dir = output / "archive_demo"
    for path in (samples_dir, metadata_dir, demo_dir):
        if path.exists():
            shutil.rmtree(path)
        path.mkdir(parents=True, exist_ok=True)
    trace_path = output / "trace.jsonl"
    if trace_path.exists():
        trace_path.unlink()
    trace = TraceLogger(trace_path)

    assets: list[dict[str, Any]] = []
    review_report: dict[str, Any] | None = None
    for spec in SPECS:
        audio = _build_audio(seed, spec.layout, spec.variant, spec.bits)
        asset_key = f"seed{seed}-{spec.suffix}"
        wav_path = samples_dir / f"{asset_key}.wav"
        write_pcm_wav(
            wav_path,
            audio,
            sample_rate=48000,
            bits=spec.bits,
            layout=spec.layout,
        )
        wav = read_wav(wav_path)
        weights = default_downmix_weights(wav.channel_layout, wav.channel_names)
        analyzer = LoudnessAnalyzer(wav, config, weights)
        v3 = _v3_metadata(wav, config, analyzer)
        v2 = _v2_metadata(wav, config, analyzer, flip_flag=True)
        integrated = analyzer.integrated_lufs()
        v1 = _v1_metadata(wav, config, integrated)
        asset_id = _asset_id_for(wav)
        v1["asset_id"] = asset_id
        v2["asset_id"] = asset_id
        v3["asset_id"] = asset_id
        v3["archive_checksum"] = sha256_bytes(
            canonical_json_bytes(
                {key: value for key, value in v3.items() if key != "archive_checksum"}
            )
        )
        metadata_paths = {
            1: metadata_dir / f"{asset_key}.v1.json",
            2: metadata_dir / f"{asset_key}.v2.json",
            3: metadata_dir / f"{asset_key}.v3.json",
        }
        atomic_write_pretty_json(metadata_paths[1], v1)
        atomic_write_pretty_json(metadata_paths[2], v2)
        atomic_write_pretty_json(metadata_paths[3], v3)
        parsed = {
            version: parse_metadata(read_json(path), wav=wav)
            for version, path in metadata_paths.items()
        }
        _trace_baseline(trace, asset_id, 1, [], segment_index_override=-1)
        _trace_baseline(trace, asset_id, 2, v2["segments"])
        _trace_baseline(trace, asset_id, 3, v3["segments"])
        asset_record = {
            "asset_id": asset_id,
            "asset_key": asset_key,
            "wav": wav_path.name,
            "wav_sha256": wav.file_sha256,
            "bits_per_sample": spec.bits,
            "channel_layout": spec.layout,
            "metadata": {
                f"v{version}": {
                    "path": f"metadata/{path.name}",
                    "sha256": _metadata_hash(read_json(path)),
                }
                for version, path in metadata_paths.items()
            },
            "v3_over_limit_segments": [
                int(row["index"]) for row in v3["segments"] if row["over_limit"]
            ],
            "v2_declared_over_limit_segments": [
                int(row["index"]) for row in v2["segments"] if row["over_limit"]
            ],
            "v1_has_segment_boundaries": False,
            "v2_has_segment_boundaries": False,
            "v3_has_segment_boundaries": True,
            "expected_requires_reworks": [
                int(row["index"]) for row in v3["segments"] if row["over_limit"]
            ],
            "review": None,
        }
        assets.append(asset_record)
        if review_report is None:
            review_report = review_versions(wav, parsed, config)
            review_report["asset_key"] = asset_key
            asset_record["review"] = {
                "code": review_report["code"],
                "first_mismatch": review_report["first_mismatch"],
            }

    demo_asset = assets[0]
    demo_wav_path = samples_dir / demo_asset["wav"]
    demo_meta_path = output / demo_asset["metadata"]["v3"]["path"]
    demo_wav = read_wav(demo_wav_path)
    demo_meta = parse_metadata(read_json(demo_meta_path), wav=demo_wav)
    service = ArchiveService(demo_dir, config, trace)

    def crash() -> None:
        raise SimulatedCrash("simulated forced termination after committed segment")

    crashed_after = 2
    try:
        service.execute(
            demo_wav,
            demo_meta,
            target_version=3,
            crash_after_commits=crashed_after,
            crash_callback=crash,
        )
    except SimulatedCrash:
        pass
    recovered = service.execute(demo_wav, demo_meta, target_version=3)
    publisher = FileVersionPublisher(demo_dir, trace)
    published = publisher.publish(
        recovered.manifest,
        [analysis.to_dict() for analysis in recovered.analyses],
        target_version=3,
        loss_manifest=recovered.loss_manifest,
        field_mapping=list(recovered.field_mapping),
        expected_manifest_chain_head=recovered.manifest["chain_head"],
    )
    query = publisher.query(
        published["version_id"],
        expected_version_hash=published["content_hash"],
    )
    baseline = {
        "schema": "loudness-generation-baseline",
        "schema_version": 1,
        "seed": seed,
        "config": config.to_dict(),
        "config_fingerprint": config.fingerprint,
        "assets": assets,
        "review_report_path": "review-report.json",
        "recovery_demo": {
            "asset_key": demo_asset["asset_key"],
            "asset_id": demo_asset["asset_id"],
            "crashed_after_commits": crashed_after,
            "manifest_revision_before_recovery": recovered.recovery["manifest_revision_before"],
            "manifest_revision_after_recovery": recovered.recovery["manifest_revision_after"],
            "committed_before_recovery": recovered.recovery["committed_before"],
            "skipped_committed_segments": recovered.recovery["skipped_committed"],
            "computed_uncommitted_segments": recovered.recovery["computed_uncommitted"],
            "chain_head": recovered.manifest["chain_head"],
        },
        "published_version": {
            "version_id": published["version_id"],
            "content_hash": published["content_hash"],
            "manifest_chain_head": published["manifest_chain_head"],
            "query_hit_version": query["hit_version"],
            "query_content_hash": query["content_hash"],
        },
        "trace_path": "trace.jsonl",
    }
    if review_report is not None:
        atomic_write_pretty_json(output / "review-report.json", review_report)
    atomic_write_pretty_json(output / "baseline.json", baseline)
    trace.flush()
    return baseline

