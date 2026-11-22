"""Validated processing and session configuration."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, replace
from typing import Any, Literal

from .errors import param_error

CHECKPOINT_SCHEMA_VERSION = 1
PIPELINE_VERSION = "stft-spectral-subtraction-v1"

WindowName = Literal["hann", "hamming", "blackman", "rectangular"]
NoiseMode = Literal["leading", "silence"]


@dataclass(frozen=True)
class RepairConfig:
    """Per-segment DSP parameters.

    Every committed segment carries the fingerprint of the config used to make
    it.  A later UI parameter change therefore cannot mutate that output.
    """

    frame_size: int = 2048
    hop_size: int = 512
    window: WindowName = "hann"
    strength: float = 1.35
    floor: float = 0.04
    noise_frames: int = 10
    noise_mode: NoiseMode = "leading"
    freq_smooth: int = 5
    temporal_smooth: float = 0.25

    def validate(self) -> "RepairConfig":
        if not isinstance(self.frame_size, int) or isinstance(self.frame_size, bool):
            raise param_error("frame_size must be an integer", field="frame_size")
        if not 64 <= self.frame_size <= 16384:
            raise param_error(
                "frame_size must be in [64, 16384]", field="frame_size", value=self.frame_size
            )
        if self.frame_size & (self.frame_size - 1):
            raise param_error(
                "frame_size must be a power of two for the radix-2 FFT pipeline",
                field="frame_size",
                value=self.frame_size,
            )
        if not isinstance(self.hop_size, int) or isinstance(self.hop_size, bool):
            raise param_error("hop_size must be an integer", field="hop_size")
        if not 1 <= self.hop_size <= self.frame_size:
            raise param_error(
                "hop_size must be in [1, frame_size]", field="hop_size", value=self.hop_size
            )
        if self.window not in ("hann", "hamming", "blackman", "rectangular"):
            raise param_error("unsupported window", field="window", value=self.window)
        if not isinstance(self.strength, (int, float)) or isinstance(self.strength, bool):
            raise param_error("strength must be numeric", field="strength")
        if not 0.0 <= float(self.strength) <= 4.0:
            raise param_error(
                "strength must be in [0, 4]", field="strength", value=self.strength
            )
        if not isinstance(self.floor, (int, float)) or isinstance(self.floor, bool):
            raise param_error("floor must be numeric", field="floor")
        if not 0.0 <= float(self.floor) <= 0.9:
            raise param_error("floor must be in [0, 0.9]", field="floor", value=self.floor)
        if not isinstance(self.noise_frames, int) or isinstance(self.noise_frames, bool):
            raise param_error("noise_frames must be an integer", field="noise_frames")
        if not 1 <= self.noise_frames <= 512:
            raise param_error(
                "noise_frames must be in [1, 512]",
                field="noise_frames",
                value=self.noise_frames,
            )
        if self.noise_mode not in ("leading", "silence"):
            raise param_error("unsupported noise_mode", field="noise_mode", value=self.noise_mode)
        if not isinstance(self.freq_smooth, int) or isinstance(self.freq_smooth, bool):
            raise param_error("freq_smooth must be an integer", field="freq_smooth")
        if not 1 <= self.freq_smooth <= 51:
            raise param_error(
                "freq_smooth must be in [1, 51]", field="freq_smooth", value=self.freq_smooth
            )
        if self.freq_smooth % 2 == 0:
            raise param_error(
                "freq_smooth must be odd", field="freq_smooth", value=self.freq_smooth
            )
        if not isinstance(self.temporal_smooth, (int, float)) or isinstance(
            self.temporal_smooth, bool
        ):
            raise param_error("temporal_smooth must be numeric", field="temporal_smooth")
        if not 0.0 <= float(self.temporal_smooth) <= 0.95:
            raise param_error(
                "temporal_smooth must be in [0, 0.95]",
                field="temporal_smooth",
                value=self.temporal_smooth,
            )
        return self

    def with_strength(self, strength: float) -> "RepairConfig":
        return replace(self, strength=float(strength)).validate()

    def canonical_dict(self) -> dict[str, Any]:
        data = asdict(self)
        data["strength"] = float(data["strength"])
        data["floor"] = float(data["floor"])
        data["temporal_smooth"] = float(data["temporal_smooth"])
        return data

    def fingerprint(self) -> str:
        payload = json.dumps(
            self.canonical_dict(), sort_keys=True, separators=(",", ":"), ensure_ascii=True
        ).encode("ascii")
        return hashlib.sha256(payload).hexdigest()


@dataclass(frozen=True)
class SessionConfig:
    segment_samples: int = 16384
    max_workers: int = 4
    max_buffer_bytes: int = 64 * 1024 * 1024

    def validate(self, channels: int) -> "SessionConfig":
        if not isinstance(self.segment_samples, int) or isinstance(
            self.segment_samples, bool
        ):
            raise param_error("segment_samples must be an integer", field="segment_samples")
        if not 256 <= self.segment_samples <= 8_000_000:
            raise param_error(
                "segment_samples must be in [256, 8000000]",
                field="segment_samples",
                value=self.segment_samples,
            )
        if not isinstance(self.max_workers, int) or isinstance(self.max_workers, bool):
            raise param_error("max_workers must be an integer", field="max_workers")
        if not 1 <= self.max_workers <= 8:
            raise param_error(
                "max_workers must be in [1, 8]", field="max_workers", value=self.max_workers
            )
        if not isinstance(self.max_buffer_bytes, int) or isinstance(
            self.max_buffer_bytes, bool
        ):
            raise param_error(
                "max_buffer_bytes must be an integer", field="max_buffer_bytes"
            )
        if self.max_buffer_bytes < 4 * 1024 * 1024:
            raise param_error(
                "max_buffer_bytes must be at least 4 MiB",
                field="max_buffer_bytes",
                value=self.max_buffer_bytes,
            )
        # Conservative peak estimate: input/output, padded frames, FFT
        # coefficients, overlap-add buffers and scheduler copies.
        estimate = self.max_workers * self.segment_samples * channels * 8 * 8
        if estimate > self.max_buffer_bytes:
            raise param_error(
                "configured parallelism exceeds the memory budget",
                field="max_buffer_bytes",
                estimated_bytes=estimate,
                limit_bytes=self.max_buffer_bytes,
            )
        return self

    def fingerprint(self) -> str:
        payload = json.dumps(asdict(self), sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(payload).hexdigest()


PROFILE_DEFAULTS: dict[str, RepairConfig] = {
    "music": RepairConfig(
        frame_size=2048,
        hop_size=512,
        window="hann",
        strength=1.35,
        floor=0.04,
        noise_frames=12,
        noise_mode="leading",
        freq_smooth=5,
        temporal_smooth=0.25,
    ),
    "speech": RepairConfig(
        frame_size=1024,
        hop_size=256,
        window="hann",
        strength=1.85,
        floor=0.06,
        noise_frames=8,
        noise_mode="leading",
        freq_smooth=7,
        temporal_smooth=0.5,
    ),
}


def default_config(profile: str) -> RepairConfig:
    try:
        return PROFILE_DEFAULTS[profile]
    except KeyError as exc:
        raise param_error("unknown audio profile", profile=profile) from exc

