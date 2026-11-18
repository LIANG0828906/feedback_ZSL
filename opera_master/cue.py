"""Cue-sheet model, parsing and structured validation."""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field

from . import (
    ALL_PARTS,
    PART_BED,
)
from .errors import (
    BED_CHANNEL_MISSING,
    BIT_DEPTH_MISMATCH,
    CHANNEL_COUNT_MISMATCH,
    CueValidationError,
    DUPLICATE_PART_NAME,
    FILE_MISSING,
    INVALID_CUE,
    PART_MISSING,
    REF_TIMES_NOT_ASCENDING,
    SAMPLE_RATE_MISMATCH,
    make_error,
)
from .wavio import read_wav


@dataclass
class ChannelSpec:
    part: str
    path: str
    crosstalk_source: str | None = None
    crosstalk_gain_db: float = 0.0
    crosstalk_window: list[float] = field(default_factory=list)
    jitter_samples: int = 0


@dataclass
class SectionSpec:
    name: str
    start: float
    end: float
    group_gains_db: dict


@dataclass
class CueSheet:
    sample_rate: int
    bit_depth: int
    duration_seconds: float
    seed: int
    reference_times: list[float]
    channels: list[ChannelSpec]
    sections: list[SectionSpec]
    target_lufs: float
    true_peak_limit_db: float
    crossfade_seconds: float
    block_samples: int
    output_master: str
    output_report: str
    snapshot_dir: str

    def channel(self, part):
        for ch in self.channels:
            if ch.part == part:
                return ch
        raise KeyError(part)

    @property
    def total_samples(self):
        return int(round(self.duration_seconds * self.sample_rate))

    def ref_samples(self):
        return [int(round(t * self.sample_rate)) for t in self.reference_times]

    def canonical_dict(self):
        return {
            "sample_rate": self.sample_rate,
            "bit_depth": self.bit_depth,
            "duration_seconds": self.duration_seconds,
            "seed": self.seed,
            "reference_times": list(self.reference_times),
            "channels": [
                {
                    "part": ch.part,
                    "path": ch.path,
                    "crosstalk_source": ch.crosstalk_source,
                    "crosstalk_gain_db": ch.crosstalk_gain_db,
                    "crosstalk_window": list(ch.crosstalk_window),
                    "jitter_samples": ch.jitter_samples,
                }
                for ch in self.channels
            ],
            "sections": [
                {
                    "name": s.name,
                    "start": s.start,
                    "end": s.end,
                    "group_gains_db": dict(s.group_gains_db),
                }
                for s in self.sections
            ],
            "target_lufs": self.target_lufs,
            "true_peak_limit_db": self.true_peak_limit_db,
            "crossfade_seconds": self.crossfade_seconds,
            "block_samples": self.block_samples,
            "output_master": self.output_master,
            "output_report": self.output_report,
            "snapshot_dir": self.snapshot_dir,
        }

    def digest(self):
        blob = json.dumps(
            self.canonical_dict(), ensure_ascii=False, sort_keys=True
        ).encode("utf-8")
        return hashlib.sha256(blob).hexdigest()


def _require(obj, key, errors, ctx):
    if key not in obj:
        errors.append(make_error(INVALID_CUE, f"{ctx}: missing '{key}'"))
        return None
    return obj[key]


def parse_cue(raw):
    errors = []
    required_scalar = [
        "sample_rate",
        "bit_depth",
        "duration_seconds",
        "seed",
        "reference_times",
        "channels",
        "sections",
        "target_lufs",
        "output_master",
        "output_report",
        "snapshot_dir",
    ]
    values = {k: _require(raw, k, errors, "cue") for k in required_scalar}
    if errors:
        raise CueValidationError(errors)

    if not isinstance(values["sample_rate"], int) or values["sample_rate"] <= 0:
        errors.append(make_error(INVALID_CUE, "sample_rate must be positive int"))
    if values["bit_depth"] not in (16, 24, 32):
        errors.append(make_error(INVALID_CUE, "bit_depth must be 16, 24 or 32"))
    if not isinstance(values["seed"], int) or values["seed"] < 0:
        errors.append(make_error(INVALID_CUE, "seed must be a non-negative int"))
    if not isinstance(values["reference_times"], list) or not values[
        "reference_times"
    ]:
        errors.append(make_error(INVALID_CUE, "reference_times must be a non-empty list"))
    if errors:
        raise CueValidationError(errors)

    ref_times = [float(t) for t in values["reference_times"]]
    if any(b <= a for a, b in zip(ref_times, ref_times[1:])):
        errors.append(
            make_error(
                REF_TIMES_NOT_ASCENDING,
                "reference times must be strictly ascending",
                details={"reference_times": ref_times},
            )
        )

    channels = []
    seen = set()
    for entry in values["channels"]:
        part = entry.get("part")
        if part in seen:
            errors.append(
                make_error(DUPLICATE_PART_NAME, f"duplicate part '{part}'", part)
            )
            continue
        seen.add(part)
        if part not in ALL_PARTS:
            errors.append(
                make_error(
                    INVALID_CUE, f"unknown part name '{part}'", part
                )
            )
        win = entry.get("crosstalk_window") or []
        channels.append(
            ChannelSpec(
                part=part,
                path=entry.get("path", ""),
                crosstalk_source=entry.get("crosstalk_source"),
                crosstalk_gain_db=float(entry.get("crosstalk_gain_db", 0.0)),
                crosstalk_window=[float(x) for x in win],
                jitter_samples=int(entry.get("jitter_samples", 0)),
            )
        )

    present = {ch.part for ch in channels}
    if PART_BED not in present:
        errors.append(
            make_error(BED_CHANNEL_MISSING, "noise bed reference channel missing")
        )
    for part in ALL_PARTS:
        if part not in present:
            errors.append(
                make_error(PART_MISSING, f"required part '{part}' missing", part)
            )

    sections = []
    for entry in values["sections"]:
        sections.append(
            SectionSpec(
                name=str(entry.get("name", "")),
                start=float(entry["start"]),
                end=float(entry["end"]),
                group_gains_db={
                    str(k): float(v) for k, v in entry["group_gains_db"].items()
                },
            )
        )
    if sections:
        bounds = [sections[0].start] + [s.end for s in sections]
        if any(b <= a for a, b in zip(bounds, bounds[1:])):
            errors.append(
                make_error(INVALID_CUE, "section boundaries must be ascending")
            )
    if errors:
        raise CueValidationError(errors)

    return CueSheet(
        sample_rate=values["sample_rate"],
        bit_depth=values["bit_depth"],
        duration_seconds=float(values["duration_seconds"]),
        seed=values["seed"],
        reference_times=ref_times,
        channels=channels,
        sections=sections,
        target_lufs=float(values["target_lufs"]),
        true_peak_limit_db=float(raw.get("true_peak_limit_db", -1.0)),
        crossfade_seconds=float(raw.get("crossfade_seconds", 0.01)),
        block_samples=int(raw.get("block_samples", 48000)),
        output_master=values["output_master"],
        output_report=values["output_report"],
        snapshot_dir=values["snapshot_dir"],
    )


def load_cue(path):
    with open(path, "r", encoding="utf-8") as fh:
        raw = json.load(fh)
    return parse_cue(raw)


def validate_wavs(cue, base_dir=None):
    """Verify every declared WAV exists and matches the cue contract."""
    errors = []
    for ch in cue.channels:
        path = ch.path
        if base_dir and not os.path.isabs(path):
            path = os.path.join(base_dir, path)
        if not os.path.isfile(path):
            errors.append(
                make_error(FILE_MISSING, f"file not found: {ch.path}", ch.part)
            )
            continue
        try:
            _, sr, bits, channels = read_wav(path)
        except Exception as exc:  # invalid container is a file-level failure
            errors.append(
                make_error(FILE_MISSING, f"unreadable wav: {exc}", ch.part)
            )
            continue
        if sr != cue.sample_rate:
            errors.append(
                make_error(
                    SAMPLE_RATE_MISMATCH,
                    f"expected {cue.sample_rate} Hz, got {sr} Hz",
                    ch.part,
                    {"expected": cue.sample_rate, "actual": sr},
                )
            )
        if bits != cue.bit_depth:
            errors.append(
                make_error(
                    BIT_DEPTH_MISMATCH,
                    f"expected {cue.bit_depth}-bit, got {bits}-bit",
                    ch.part,
                    {"expected": cue.bit_depth, "actual": bits},
                )
            )
        if channels != 1:
            errors.append(
                make_error(
                    CHANNEL_COUNT_MISMATCH,
                    f"expected 1 channel, got {channels}",
                    ch.part,
                    {"expected": 1, "actual": channels},
                )
            )
    if errors:
        raise CueValidationError(errors)
