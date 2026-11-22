from __future__ import annotations

from dataclasses import dataclass

PROTOCOL_VERSION = "scwb/1.0"
SCHEMA_VERSION = 1


@dataclass(frozen=True)
class CodecProfile:
    name: str
    rank: int
    fragment_bytes: int
    compression_level: int
    label: str


CODEC_PROFILES: dict[str, CodecProfile] = {
    "h264-baseline": CodecProfile(
        "h264-baseline", 10, 420, 1, "H.264 Baseline"
    ),
    "h264-high": CodecProfile("h264-high", 20, 640, 3, "H.264 High"),
    "hevc-main": CodecProfile("hevc-main", 30, 860, 6, "HEVC Main"),
}

CODEC_ORDER = tuple(
    profile.name for profile in sorted(CODEC_PROFILES.values(), key=lambda p: p.rank)
)


def codec_profile(name: str) -> CodecProfile:
    return CODEC_PROFILES[name]
