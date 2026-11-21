"""Peak-memory estimation and immutable budget validation."""

from __future__ import annotations

from dataclasses import dataclass

from .config import MEMORY_PROFILES, MemoryConfig
from .errors import E_BUDGET_EXCEEDED, error
from .model import SequenceHeader


@dataclass(frozen=True)
class MemoryEstimate:
    profile: str
    sample_count: int
    float_buffer_count: int
    float_bytes: int
    encoded_bytes: int
    overhead_bytes: int
    total_bytes: int

    def to_dict(self) -> dict[str, int | str]:
        return {
            "profile": self.profile,
            "sample_count": self.sample_count,
            "float_buffer_count": self.float_buffer_count,
            "float_bytes": self.float_bytes,
            "encoded_bytes": self.encoded_bytes,
            "overhead_bytes": self.overhead_bytes,
            "total_bytes": self.total_bytes,
        }


def estimate_tile_memory(
    header: SequenceHeader,
    profile: str,
    overhead_bytes: int = 12_288,
) -> MemoryEstimate:
    if profile not in MEMORY_PROFILES:
        raise error(E_BUDGET_EXCEEDED, "unknown memory downgrade profile", field="profile", actual=profile)
    if header.bit_depth > 8:
        encoded_bytes = header.tile_width * header.tile_height * header.channels * 2
    else:
        encoded_bytes = header.tile_width * header.tile_height * header.channels
    sample_count = header.tile_width * header.tile_height * header.channels
    if profile == "full":
        # Source decode, XYZ, target RGB, tone map input/output and quantization staging.
        float_samples = sample_count * 6
    elif profile == "banded":
        band_rows = max(1, min(header.tile_height, 8))
        float_samples = band_rows * header.tile_width * header.channels * 4
    else:
        # One scanline, reused in place. Precision remains float64 and output bit depth is unchanged.
        float_samples = header.tile_width * header.channels * 2
    float_bytes = float_samples * 8
    total = float_bytes + encoded_bytes + overhead_bytes
    return MemoryEstimate(
        profile=profile,
        sample_count=sample_count,
        float_buffer_count=MEMORY_PROFILES[profile],
        float_bytes=float_bytes,
        encoded_bytes=encoded_bytes,
        overhead_bytes=overhead_bytes,
        total_bytes=total,
    )


def choose_profile(
    header: SequenceHeader,
    config: MemoryConfig,
) -> tuple[MemoryEstimate, list[MemoryEstimate]]:
    """Return the first declared profile fitting the immutable tile budget.

    Every rejected attempt is returned so the caller can emit a downgrade trace.
    """

    attempts: list[MemoryEstimate] = []
    for profile in config.downgrade_profiles:
        estimate = estimate_tile_memory(header, profile, config.estimator_overhead_bytes)
        attempts.append(estimate)
        if estimate.total_bytes <= config.tile_budget_bytes:
            return estimate, attempts
    last = attempts[-1] if attempts else None
    raise error(
        E_BUDGET_EXCEEDED,
        "tile exceeds immutable memory budget after all downgrade stages",
        field="memory.tile_budget_bytes",
        budget_bytes=config.tile_budget_bytes,
        attempts=[attempt.to_dict() for attempt in attempts],
        last_total_bytes=last.total_bytes if last is not None else None,
        precision_changed=False,
        bit_depth=header.bit_depth,
    )
