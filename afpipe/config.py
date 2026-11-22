"""Pipeline configuration, version identifiers and parameter fingerprints.

Versioning model (explicit, never silently mixed):

* ``INDEX_FORMAT_VERSION``  - on-disk layout of the index store.
* ``ALGORITHM_VERSION``     - fingerprint extraction algorithm.
* :meth:`Config.parameter_version` - hash of every extraction / retrieval
  parameter that affects fingerprints; a different hash means stored
  fingerprints are incompatible with a new query run.
"""

from __future__ import annotations

import dataclasses
import math

from .errors import ParameterError
from .hashing import stable_digest

# Bumped whenever the on-disk index layout changes.
INDEX_FORMAT_VERSION = "2.0.0"
# The only legacy layout the upgrader knows how to read.
LEGACY_INDEX_FORMAT_VERSION = "1.0.0"
# Bumped whenever the fingerprint algorithm itself changes.
ALGORITHM_VERSION = "1.0.0"

# Quantisation levels supported by the nested quantiser (bits per delta).
SUPPORTED_QUANT_BITS = (1, 2, 4)


@dataclasses.dataclass(frozen=True)
class Config:
    """All tunable parameters with hard validation in :meth:`validate`."""

    # Decode / normalisation.
    sample_rate: int = 16000
    channel_mode: str = "mono"          # "mono" (downmix) or "keep"
    max_channels: int = 2
    duration_tolerance_s: float = 0.06  # manifest vs decoded duration
    decode_buf_frames: int = 1_048_576  # per-thread sample buffer cap

    # Framing / fingerprinting.
    frame_ms: float = 46.4              # 742 samples @16k
    hop_ms: float = 23.2                # 371 samples @16k
    bands: int = 32
    quant_bits: int = 2
    # Minimum fraction of identical frame hashes two copies must share.
    match_rate_floor: float = 0.75

    # Retrieval.
    min_query_ms: float = 300.0
    max_query_ms: float = 30_000.0
    max_postings_scan: int = 200_000    # per query work budget
    min_aligned_frames: int = 6
    family_score_floor: float = 0.12

    # Build / resources.
    shard_frames: int = 256             # frames persisted per checkpoint shard
    max_workers: int = 2

    def validate(self) -> "Config":
        """Raise :class:`E_PARA` on any out-of-range value."""
        checks = [
            (isinstance(self.sample_rate, int) and 8000 <= self.sample_rate <= 192000,
             "sample_rate must be an int in [8000, 192000]"),
            (self.channel_mode in ("mono", "keep"),
             "channel_mode must be 'mono' or 'keep'"),
            (isinstance(self.max_channels, int) and 1 <= self.max_channels <= 8,
             "max_channels must be an int in [1, 8]"),
            (self.duration_tolerance_s >= 0,
             "duration_tolerance_s must be >= 0"),
            (self.decode_buf_frames >= 16384,
             "decode_buf_frames must be >= 16384"),
            (self.frame_ms > 0 and self.hop_ms > 0,
             "frame_ms and hop_ms must be positive"),
            (self.hop_ms <= self.frame_ms,
             "hop_ms must be <= frame_ms"),
            (self.frame_ms * self.sample_rate / 1000.0 >= 16,
             "frame must contain at least 16 samples"),
            (isinstance(self.bands, int) and 4 <= self.bands <= 128,
             "bands must be an int in [4, 128]"),
            (self.quant_bits in SUPPORTED_QUANT_BITS,
             f"quant_bits must be one of {SUPPORTED_QUANT_BITS}"),
            (0.0 <= self.match_rate_floor <= 1.0,
             "match_rate_floor must be in [0, 1]"),
            (self.min_query_ms > 0,
             "min_query_ms must be positive"),
            (self.max_query_ms >= self.min_query_ms,
             "max_query_ms must be >= min_query_ms"),
            (self.max_postings_scan > 0,
             "max_postings_scan must be positive"),
            (self.min_aligned_frames >= 1,
             "min_aligned_frames must be >= 1"),
            (0.0 <= self.family_score_floor <= 1.0,
             "family_score_floor must be in [0, 1]"),
            (isinstance(self.shard_frames, int) and self.shard_frames >= 32,
             "shard_frames must be an int >= 32"),
            (isinstance(self.max_workers, int) and 1 <= self.max_workers <= 16,
             "max_workers must be an int in [1, 16]"),
        ]
        for ok, message in checks:
            if not ok:
                raise ParameterError(message, details={"field": message.split()[0]})
        nfft = self.frame_samples()
        if self.bands * 2 > nfft // 2 + 1:
            raise ParameterError(
                "too many bands for frame size",
                details={"bands": self.bands, "frame_samples": nfft},
            )
        return self

    def frame_samples(self) -> int:
        return int(round(self.frame_ms * self.sample_rate / 1000.0))

    def hop_samples(self) -> int:
        return int(round(self.hop_ms * self.sample_rate / 1000.0))

    def parameter_version(self) -> str:
        """Deterministic fingerprint of every fingerprint-affecting value."""
        payload = {
            "algo": ALGORITHM_VERSION,
            "sample_rate": self.sample_rate,
            "channel_mode": self.channel_mode,
            "max_channels": self.max_channels,
            "frame_ms": self.frame_ms,
            "hop_ms": self.hop_ms,
            "bands": self.bands,
            "quant_bits": self.quant_bits,
            "frame_samples": self.frame_samples(),
            "hop_samples": self.hop_samples(),
        }
        return stable_digest(payload, prefix="pv", length=16)

    def short_label(self) -> str:
        return f"sr{self.sample_rate}_b{self.bands}_q{self.quant_bits}"

    def with_overrides(self, **overrides) -> "Config":
        return dataclasses.replace(self, **overrides)

    def duration_of_frames(self, frame_count: int) -> float:
        """Wall-clock span covered by ``frame_count`` hops."""
        if frame_count <= 0:
            return 0.0
        return (frame_count - 1) * self.hop_samples() / self.sample_rate + (
            self.frame_samples() / self.sample_rate
        )

    def expected_frame_count(self, duration_s: float) -> int:
        n = self.frame_samples()
        h = self.hop_samples()
        if duration_s * self.sample_rate < n:
            return 0
        return int(math.floor((duration_s * self.sample_rate - n) / h)) + 1
