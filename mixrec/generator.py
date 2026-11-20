"""Deterministic local WAV fixtures, job configs and baseline results."""

from __future__ import annotations

import hashlib
import json
import math
import os
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .engine import MixEngine, summarize_index
from .index import atomic_write_json, load_index
from .wavio import wav_bytes


@dataclass(frozen=True)
class SampleTrack:
    id: str
    sample_rate: int
    channels: int
    gain_db: float = 0.0
    delay_samples: int = 0
    mute_ranges: tuple[tuple[float, float], ...] = ()


@dataclass(frozen=True)
class SampleJob:
    job_id: str
    output_rate: int
    output_channels: int
    slice_samples: int
    total_samples: int
    tracks: tuple[SampleTrack, ...]
    max_workers: int = 2
    per_slice_budget_bytes: int = 64 * 1024 * 1024
    memory_budget_bytes: int = 128 * 1024 * 1024


REFERENCE_JOBS: tuple[SampleJob, ...] = (
    SampleJob(
        job_id="mixed_rates_mono",
        output_rate=48000,
        output_channels=1,
        slice_samples=4096,
        total_samples=60960,
        tracks=(
            SampleTrack("commentary", 16000, 1, gain_db=1.5),
            SampleTrack(
                "ambience",
                48000,
                2,
                gain_db=-3.0,
                delay_samples=31,
                mute_ranges=((0.38, 0.52),),
            ),
            SampleTrack(
                "player",
                16000,
                1,
                delay_samples=73,
                mute_ranges=((0.75, 0.82),),
            ),
        ),
        max_workers=2,
        per_slice_budget_bytes=16 * 1024 * 1024,
        memory_budget_bytes=32 * 1024 * 1024,
    ),
    SampleJob(
        job_id="mixed_rates_stereo",
        output_rate=16000,
        output_channels=2,
        slice_samples=1600,
        total_samples=28480,
        tracks=(
            SampleTrack("commentary", 48000, 1, gain_db=0.5, delay_samples=17),
            SampleTrack(
                "ambience",
                16000,
                2,
                gain_db=-4.5,
                mute_ranges=((0.42, 0.51),),
            ),
            SampleTrack(
                "player",
                48000,
                2,
                gain_db=2.0,
                delay_samples=43,
                mute_ranges=((0.66, 0.73), (1.12, 1.2)),
            ),
        ),
        max_workers=2,
        per_slice_budget_bytes=16 * 1024 * 1024,
        memory_budget_bytes=32 * 1024 * 1024,
    ),
    SampleJob(
        job_id="short_timeline",
        output_rate=48000,
        output_channels=1,
        slice_samples=1000,
        total_samples=4387,
        tracks=(
            SampleTrack("commentary", 16000, 1, gain_db=-1.0, delay_samples=7),
            SampleTrack("ambience", 48000, 1, delay_samples=19, mute_ranges=((0.03, 0.041),)),
            SampleTrack("player", 16000, 1, gain_db=-2.0, delay_samples=29),
        ),
        max_workers=2,
        per_slice_budget_bytes=64 * 1024,
        memory_budget_bytes=128 * 1024,
    ),
)


def _seed_for(seed: int, job_id: str, track_id: str) -> int:
    digest = hashlib.sha256(f"{seed}:{job_id}:{track_id}".encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big")


def _make_signal(
    *,
    seed: int,
    job_id: str,
    track: SampleTrack,
    frames: int,
) -> np.ndarray:
    rng = np.random.default_rng(_seed_for(seed, job_id, track.id))
    t = np.arange(frames, dtype=np.float64) / float(track.sample_rate)
    if track.id == "commentary":
        slow = 0.62 + 0.28 * np.sin(2.0 * np.pi * 1.7 * t + 0.3)
        gate = 0.55 + 0.45 * np.sin(2.0 * np.pi * 0.37 * t) ** 2
        carrier = (
            0.19 * np.sin(2.0 * np.pi * 223.0 * t + 0.15 * np.sin(2.0 * np.pi * 3.1 * t))
            + 0.08 * np.sin(2.0 * np.pi * 447.0 * t)
            + 0.04 * np.sin(2.0 * np.pi * 691.0 * t + 0.8)
        )
        mono = slow * gate * carrier
        noise = 0.016 * rng.standard_normal(frames)
        mono += noise
    elif track.id == "ambience":
        mono = (
            0.10 * np.sin(2.0 * np.pi * 92.0 * t)
            + 0.055 * np.sin(2.0 * np.pi * 311.0 * t + 0.4)
            + 0.035 * np.sin(2.0 * np.pi * 733.0 * t)
            + 0.028 * rng.standard_normal(frames)
        )
        # Slow spatial movement makes stereo up/down mixing visible while
        # remaining deterministic and comfortably inside the PCM range.
        mono *= 0.82 + 0.18 * np.sin(2.0 * np.pi * 0.09 * t)
    else:
        burst = (np.sin(2.0 * np.pi * 0.83 * t + 0.1) > -0.25).astype(np.float64)
        mono = (
            0.14 * burst * np.sin(2.0 * np.pi * 181.0 * t)
            + 0.06 * np.sin(2.0 * np.pi * 362.0 * t + 0.2)
            + 0.022 * rng.standard_normal(frames)
        )
    if track.channels == 1:
        values = mono[:, None]
    else:
        right = np.roll(mono, 5) * 0.96
        right = right + 0.012 * rng.standard_normal(frames)
        values = np.stack([mono, right], axis=1)
    values = np.clip(values, -0.999, 0.999)
    return np.rint(values * 32767.0).astype("<i2")


def _source_frames(job: SampleJob, track: SampleTrack) -> int:
    max_delay = max(item.delay_samples for item in job.tracks)
    seconds = (job.total_samples + max_delay) / float(job.output_rate)
    # A small tail ensures the final slice has real source material rather than
    # relying solely on the edge-clamped interpolation boundary.
    return int(math.ceil(seconds * track.sample_rate)) + track.sample_rate // 20


def _write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + f".tmp-{os.getpid()}")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _job_config(job: SampleJob) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "job_id": job.job_id,
        "output": {
            "sample_rate": job.output_rate,
            "channels": job.output_channels,
            "bit_depth": 16,
            "slice_samples": job.slice_samples,
            "total_samples": job.total_samples,
        },
        "tracks": [
            {
                "id": track.id,
                "path": f"tracks/{track.id}.wav",
                "sample_rate": track.sample_rate,
                "channels": track.channels,
                "gain_db": track.gain_db,
                "mute_ranges": [list(item) for item in track.mute_ranges],
                "delay_samples": track.delay_samples,
                "version": 1,
                "max_delay_samples": 48000,
            }
            for track in job.tracks
        ],
        "scheduler": {
            "max_workers": job.max_workers,
            "per_slice_budget_bytes": job.per_slice_budget_bytes,
            "memory_budget_bytes": job.memory_budget_bytes,
            "block_samples": job.slice_samples,
        },
        "output_dir": "output",
        "trace_path": "render_trace.jsonl",
        "max_delay_samples": 48000,
    }


def generate_samples(
    out_dir: str | os.PathLike[str],
    *,
    seed: int = 20261121,
    with_baseline: bool = True,
    jobs: tuple[SampleJob, ...] = REFERENCE_JOBS,
) -> dict[str, Any]:
    """Generate WAVs, configs, manifest and optionally reference baselines."""

    root = Path(out_dir)
    root.mkdir(parents=True, exist_ok=True)
    manifest_jobs: list[dict[str, Any]] = []
    for job in jobs:
        job_dir = root / "jobs" / job.job_id
        if job_dir.exists():
            shutil.rmtree(job_dir)
        job_dir.mkdir(parents=True, exist_ok=True)
        for track in job.tracks:
            frames = _source_frames(job, track)
            pcm = _make_signal(seed=seed, job_id=job.job_id, track=track, frames=frames)
            _write_bytes(job_dir / "tracks" / f"{track.id}.wav", wav_bytes(pcm, track.sample_rate, track.channels))
        atomic_write_json(job_dir / "config.json", _job_config(job))
        manifest_jobs.append(
            {
                "config": f"jobs/{job.job_id}/config.json",
                "duration_samples": job.total_samples,
                "job_id": job.job_id,
                "output_channels": job.output_channels,
                "output_rate": job.output_rate,
                "slice_samples": job.slice_samples,
                "track_channels": [track.channels for track in job.tracks],
                "track_rates": [track.sample_rate for track in job.tracks],
            }
        )
    manifest = {
        "schema_version": 1,
        "generator": "mixrec.generator",
        "seed": seed,
        "jobs": manifest_jobs,
    }
    atomic_write_json(root / "manifest.json", manifest)
    if with_baseline:
        for job in jobs:
            write_baseline(job, root)
    return manifest


def write_baseline(job: SampleJob, root: Path) -> dict[str, Any]:
    job_dir = root / "jobs" / job.job_id
    config_path = job_dir / "config.json"
    engine = MixEngine.from_file(config_path)
    result = engine.run(resume=False, fresh=False, trace_path=job_dir / "render_trace.jsonl")
    index = result.index or load_index(engine.index_path)
    slices = index.get("slices", [])
    trace_hash = hashlib.sha256((job_dir / "render_trace.jsonl").read_bytes()).hexdigest()
    baseline = {
        "schema_version": 1,
        "job_id": job.job_id,
        "config": f"jobs/{job.job_id}/config.json",
        "index_version": int(index["index_version"]),
        "chain_head": index["chain_head"],
        "slice_count": len(slices),
        "slice_content_sha256": [item["content_sha256"] for item in slices],
        "slice_file_sha256": [item["file_sha256"] for item in slices],
        "trace_sha256": trace_hash,
    }
    atomic_write_json(root / "baseline" / f"{job.job_id}.json", baseline)
    return baseline


def baseline_from_index(
    *,
    root: Path,
    job_id: str,
    config_relative: str,
    index: dict[str, Any],
    trace_path: Path,
) -> dict[str, Any]:
    slices = index.get("slices", [])
    baseline = {
        "schema_version": 1,
        "job_id": job_id,
        "config": config_relative,
        "index_version": int(index["index_version"]),
        "chain_head": index["chain_head"],
        "slice_count": len(slices),
        "slice_content_sha256": [item["content_sha256"] for item in slices],
        "slice_file_sha256": [item["file_sha256"] for item in slices],
        "trace_sha256": hashlib.sha256(trace_path.read_bytes()).hexdigest(),
    }
    atomic_write_json(root / "baseline" / f"{job_id}.json", baseline)
    return baseline


__all__ = [
    "REFERENCE_JOBS",
    "SampleJob",
    "SampleTrack",
    "baseline_from_index",
    "generate_samples",
    "summarize_index",
    "write_baseline",
]


def main(argv: list[str] | None = None) -> int:
    import argparse
    import sys

    parser = argparse.ArgumentParser(description="Generate deterministic local WAV fixtures")
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=20261121)
    parser.add_argument("--no-baseline", action="store_true")
    args = parser.parse_args(argv)
    manifest = generate_samples(
        args.out,
        seed=args.seed,
        with_baseline=not args.no_baseline,
    )
    print(json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
