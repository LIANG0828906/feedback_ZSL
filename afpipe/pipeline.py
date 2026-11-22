"""Decode/normalise orchestration and manifest cross-checks."""

from __future__ import annotations

import os
from dataclasses import dataclass

from .config import Config
from .errors import ChannelError, DecodeError, MetaError
from .ffmpeg import AudioData, decode, probe
from .fingerprint import FingerprintTrack, extract
from .hashing import file_sha256
from .manifest import MediaFile, Manifest


@dataclass
class ProcessedFile:
    media: MediaFile
    audio: AudioData
    track: FingerprintTrack
    content_hash: str


def check_manifest_against_probe(media: MediaFile, manifest: Manifest) -> dict:
    """Validate declared sr/channels/duration against the real stream."""
    path = manifest.absolute_path(media)
    try:
        info = probe(path)
    except DecodeError:
        raise
    problems: list[dict] = []
    if info["sample_rate"] != media.sample_rate:
        problems.append({"field": "sample_rate",
                         "declared": media.sample_rate,
                         "actual": info["sample_rate"]})
    if info["channels"] != media.channels:
        problems.append({"field": "channels",
                         "declared": media.channels,
                         "actual": info["channels"]})
    if info["duration_s"] is not None:
        delta = abs(info["duration_s"] - media.duration_s)
        if delta > 0.20:  # probe-side tolerance (encoder padding)
            problems.append({"field": "duration_s",
                             "declared": media.duration_s,
                             "actual": round(info["duration_s"], 6),
                             "delta": round(delta, 6)})
    if problems:
        raise MetaError("manifest metadata mismatch",
                        details={"file_id": media.file_id, "problems": problems})
    return info


def process_file(media: MediaFile, manifest: Manifest,
                 cfg: Config) -> ProcessedFile:
    """Full single-file path: integrity -> probe checks -> decode -> fingerprints.

    Raises E_DECODE / E_CHAN / E_META; callers decide failure isolation.
    """
    cfg.validate()
    path = manifest.absolute_path(media)
    if not os.path.isfile(path):
        raise DecodeError("source file missing",
                          details={"file_id": media.file_id, "path": path})

    check_manifest_against_probe(media, manifest)

    if media.channels > cfg.max_channels:
        raise ChannelError(
            "channel count exceeds declared cap",
            details={"file_id": media.file_id,
                     "actual_channels": media.channels,
                     "max_channels": cfg.max_channels},
        )

    content_hash = file_sha256(path)
    if content_hash != media.sha256:
        raise MetaError(
            "source content hash disagrees with manifest",
            details={"file_id": media.file_id, "field": "sha256",
                     "declared": media.sha256, "actual": content_hash},
        )

    audio = decode(
        path,
        target_rate=cfg.sample_rate,
        channel_mode=cfg.channel_mode,
        max_channels=cfg.max_channels,
        decode_buf_frames=cfg.decode_buf_frames,
    )

    # Decoded duration (after resample) must still sit inside tolerance.
    delta = abs(audio.duration_s - media.duration_s)
    if delta > cfg.duration_tolerance_s:
        raise MetaError(
            "decoded duration outside tolerance",
            details={"file_id": media.file_id, "field": "duration_s",
                     "declared": media.duration_s,
                     "actual": round(audio.duration_s, 6),
                     "delta": round(delta, 6),
                     "tolerance": cfg.duration_tolerance_s},
        )

    track = extract(audio.samples, cfg, file_id=media.file_id)
    return ProcessedFile(media=media, audio=audio, track=track,
                         content_hash=content_hash)
