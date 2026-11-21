"""Immutable input/output models and canonical serialization helpers."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from fractions import Fraction
from functools import cached_property
from typing import Any, Iterable, Literal


ItemKind = Literal["speech", "description"]


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def content_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def fraction_text(value: Fraction) -> str:
    if value.denominator == 1:
        return str(value.numerator)
    return f"{value.numerator}/{value.denominator}"


def fraction_display_text(value: Fraction) -> str:
    """Exact decimal for finite fractions, compact ratio otherwise."""
    denominator = value.denominator
    reduced = denominator
    while reduced % 2 == 0:
        reduced //= 2
    while reduced % 5 == 0:
        reduced //= 5
    if reduced != 1:
        return fraction_text(value)
    places = 0
    scale = 1
    while scale % denominator != 0:
        scale *= 10
        places += 1
    signed = value.numerator * (scale // denominator)
    sign = "-" if signed < 0 else ""
    digits = str(abs(signed)).rjust(places + 1, "0")
    integer = digits[:-places] if places else digits
    decimal = digits[-places:].rstrip("0") if places else ""
    return f"{sign}{integer}.{decimal}" if decimal else f"{sign}{integer}"


def ceil_fraction(value: Fraction) -> int:
    return -((-value.numerator) // value.denominator)


@dataclass(frozen=True)
class SourceItem:
    item_id: str
    kind: ItemKind
    text: str
    start_sample: int
    end_sample: int
    speaker: str
    priority: int
    max_cps: Fraction
    source_rate_cps: Fraction
    min_gap_samples: int = 0

    @property
    def text_length(self) -> int:
        return len(self.text.strip())

    def minimum_samples(self, sample_rate: int) -> int:
        if self.max_cps <= 0 or self.text_length == 0:
            return 0
        return ceil_fraction(Fraction(self.text_length * sample_rate, 1) / self.max_cps)

    @property
    def nominal_duration(self) -> int:
        return max(0, self.end_sample - self.start_sample)

    def canonical(self) -> dict[str, Any]:
        return {
            "id": self.item_id,
            "kind": self.kind,
            "text": self.text,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "speaker": self.speaker,
            "priority": self.priority,
            "max_cps": fraction_text(self.max_cps),
            "source_rate_cps": fraction_text(self.source_rate_cps),
            "min_gap_samples": self.min_gap_samples,
        }

    @property
    def fingerprint(self) -> str:
        return content_hash(self.canonical())


@dataclass(frozen=True)
class SourceBundle:
    material_id: str
    sample_rate: int
    frame_table_path: str
    wav_path: str
    items: tuple[SourceItem, ...]
    params_version: str
    min_readable_ratio: Fraction = Fraction(1, 1)
    max_iterations: int = 256

    @cached_property
    def source_hash(self) -> str:
        frames_hash = ""
        try:
            with open(self.frame_table_path, "rb") as fh:
                frames_hash = hashlib.sha256(fh.read()).hexdigest()
        except OSError:
            frames_hash = "unreadable"
        audio_hash = ""
        try:
            with open(self.wav_path, "rb") as fh:
                audio_hash = hashlib.sha256(fh.read()).hexdigest()
        except OSError:
            audio_hash = "unreadable"
        return content_hash(
            {
                "material_id": self.material_id,
                "sample_rate": self.sample_rate,
                "params_version": self.params_version,
                "min_readable_ratio": fraction_text(self.min_readable_ratio),
                "max_iterations": self.max_iterations,
                "frame_table_sha256": frames_hash,
                "wav_sha256": audio_hash,
                "items": [item.canonical() for item in self.items],
            }
        )

    def canonical(self) -> dict[str, Any]:
        return {
            "material_id": self.material_id,
            "sample_rate": self.sample_rate,
            "frame_table_path": self.frame_table_path,
            "wav_path": self.wav_path,
            "params_version": self.params_version,
            "min_readable_ratio": fraction_text(self.min_readable_ratio),
            "max_iterations": self.max_iterations,
            "source_hash": self.source_hash,
            "items": [item.canonical() for item in self.items],
        }


@dataclass
class ConflictRecord:
    iteration: int
    chain: list[str]
    left_id: str
    right_id: str
    action: str
    winner_id: str | None
    loser_id: str | None
    reason: str
    left_before: tuple[int, int]
    right_before: tuple[int, int]

    def to_dict(self) -> dict[str, Any]:
        return {
            "iteration": self.iteration,
            "chain": list(self.chain),
            "left_id": self.left_id,
            "right_id": self.right_id,
            "action": self.action,
            "winner_id": self.winner_id,
            "loser_id": self.loser_id,
            "reason": self.reason,
            "left_before": list(self.left_before),
            "right_before": list(self.right_before),
        }


@dataclass
class ItemResult:
    item_id: str
    kind: ItemKind
    text: str
    speaker: str
    priority: int
    window_start: int
    window_end: int
    start: int
    end: int
    minimum_duration: int
    readable: bool
    status: str
    decision: str
    frame_start: int
    frame_end: int
    conflicts: list[dict[str, Any]] = field(default_factory=list)
    depends_on: list[str] = field(default_factory=list)
    input_fingerprint: str = ""
    result_hash: str = ""
    text_length: int = 0
    max_reading_rate_cps: str = ""
    estimated_reading_rate_cps: str = ""

    @property
    def duration(self) -> int:
        return max(0, self.end - self.start)

    def canonical(self, include_hash: bool = True) -> dict[str, Any]:
        value = {
            "id": self.item_id,
            "kind": self.kind,
            "text": self.text,
            "speaker": self.speaker,
            "priority": self.priority,
            "window_start": self.window_start,
            "window_end": self.window_end,
            "start": self.start,
            "end": self.end,
            "duration": self.duration,
            "minimum_duration": self.minimum_duration,
            "text_length": self.text_length,
            "max_reading_rate_cps": self.max_reading_rate_cps,
            "estimated_reading_rate_cps": self.estimated_reading_rate_cps,
            "readable": self.readable,
            "status": self.status,
            "decision": self.decision,
            "frame_start": self.frame_start,
            "frame_end": self.frame_end,
            "conflicts": self.conflicts,
            "depends_on": list(self.depends_on),
            "input_fingerprint": self.input_fingerprint,
        }
        if include_hash:
            value["result_hash"] = self.result_hash
        return value

    def finalize_hash(self) -> None:
        self.result_hash = content_hash(self.canonical(include_hash=False))

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "ItemResult":
        obj = cls(
            item_id=value["id"],
            kind=value["kind"],
            text=value["text"],
            speaker=value["speaker"],
            priority=int(value["priority"]),
            window_start=int(value["window_start"]),
            window_end=int(value["window_end"]),
            start=int(value["start"]),
            end=int(value["end"]),
            minimum_duration=int(value["minimum_duration"]),
            readable=bool(value["readable"]),
            status=value["status"],
            decision=value["decision"],
            frame_start=int(value["frame_start"]),
            frame_end=int(value["frame_end"]),
            conflicts=list(value.get("conflicts", [])),
            depends_on=list(value.get("depends_on", [])),
            input_fingerprint=value.get("input_fingerprint", ""),
            result_hash=value.get("result_hash", ""),
            text_length=int(value.get("text_length", 0)),
            max_reading_rate_cps=value.get("max_reading_rate_cps", ""),
            estimated_reading_rate_cps=value.get("estimated_reading_rate_cps", ""),
        )
        if not obj.result_hash:
            obj.finalize_hash()
        return obj


@dataclass
class SolveResult:
    version: int
    material_id: str
    params_version: str
    sample_rate: int
    source_hash: str
    items: list[ItemResult]
    conflicts: list[ConflictRecord]
    affected_ids: list[str]
    recomputed_ids: list[str]
    reused_ids: list[str]
    conservative_reasons: dict[str, str]
    iterations: int
    stable: bool
    result_hash: str = ""
    trace_events: list[dict[str, Any]] = field(default_factory=list)

    def by_id(self) -> dict[str, ItemResult]:
        return {item.item_id: item for item in self.items}

    def canonical(self, include_hash: bool = True) -> dict[str, Any]:
        value = {
            "version": self.version,
            "material_id": self.material_id,
            "params_version": self.params_version,
            "sample_rate": self.sample_rate,
            "source_hash": self.source_hash,
            "items": [item.canonical() for item in self.items],
            "conflicts": [record.to_dict() for record in self.conflicts],
            "affected_ids": list(self.affected_ids),
            "recomputed_ids": list(self.recomputed_ids),
            "reused_ids": list(self.reused_ids),
            "conservative_reasons": dict(self.conservative_reasons),
            "iterations": self.iterations,
            "stable": self.stable,
            "trace_events": list(self.trace_events),
        }
        if include_hash:
            value["result_hash"] = self.result_hash
        return value

    def finalize_hash(self) -> None:
        self.result_hash = content_hash(self.canonical(include_hash=False))

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "SolveResult":
        obj = cls(
            version=int(value["version"]),
            material_id=value["material_id"],
            params_version=value["params_version"],
            sample_rate=int(value["sample_rate"]),
            source_hash=value["source_hash"],
            items=[ItemResult.from_dict(item) for item in value["items"]],
            conflicts=[_conflict_from_dict(item) for item in value.get("conflicts", [])],
            affected_ids=list(value.get("affected_ids", [])),
            recomputed_ids=list(value.get("recomputed_ids", [])),
            reused_ids=list(value.get("reused_ids", [])),
            conservative_reasons=dict(value.get("conservative_reasons", {})),
            iterations=int(value.get("iterations", 0)),
            stable=bool(value.get("stable", True)),
            result_hash=value.get("result_hash", ""),
            trace_events=list(value.get("trace_events", [])),
        )
        if not obj.result_hash:
            obj.finalize_hash()
        return obj


def _conflict_from_dict(value: dict[str, Any]) -> ConflictRecord:
    return ConflictRecord(
        iteration=int(value["iteration"]),
        chain=list(value["chain"]),
        left_id=value["left_id"],
        right_id=value["right_id"],
        action=value["action"],
        winner_id=value.get("winner_id"),
        loser_id=value.get("loser_id"),
        reason=value["reason"],
        left_before=tuple(value["left_before"]),
        right_before=tuple(value["right_before"]),
    )


@dataclass
class ComplianceReport:
    material_id: str
    version: int
    sample_rate: int
    result_hash: str
    rules: dict[str, Any]
    descriptions: list[dict[str, Any]]
    violations: list[dict[str, Any]]
    compliant: bool
    report_hash: str = ""

    def canonical(self, include_hash: bool = True) -> dict[str, Any]:
        value = {
            "schema_version": 1,
            "material_id": self.material_id,
            "version": self.version,
            "sample_rate": self.sample_rate,
            "result_hash": self.result_hash,
            "rules": self.rules,
            "descriptions": self.descriptions,
            "violations": self.violations,
            "compliant": self.compliant,
        }
        if include_hash:
            value["report_hash"] = self.report_hash
        return value

    def finalize_hash(self) -> None:
        self.report_hash = content_hash(self.canonical(include_hash=False))


@dataclass
class SpeechTrack:
    material_id: str
    version: int
    sample_rate: int
    result_hash: str
    items: list[dict[str, Any]]

    def canonical(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "track": "speech",
            "material_id": self.material_id,
            "version": self.version,
            "sample_rate": self.sample_rate,
            "result_hash": self.result_hash,
            "items": self.items,
        }


@dataclass
class DescriptionTrack:
    material_id: str
    version: int
    sample_rate: int
    result_hash: str
    items: list[dict[str, Any]]

    def canonical(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "track": "description",
            "material_id": self.material_id,
            "version": self.version,
            "sample_rate": self.sample_rate,
            "result_hash": self.result_hash,
            "items": self.items,
        }


def items_to_track(
    items: Iterable[ItemResult],
    sample_rate: int,
    material_id: str,
    version: int,
    result_hash: str,
    kind: ItemKind,
) -> list[dict[str, Any]]:
    track: list[dict[str, Any]] = []
    matching = [item for item in items if item.kind == kind]
    for item in sorted(matching, key=lambda value: (value.start, value.end, value.item_id)):
        track.append(
            {
                "id": item.item_id,
                "text": item.text,
                "speaker": item.speaker,
                "start_sample": item.start,
                "end_sample": item.end,
                "start_timecode": samples_to_timecode(item.start, sample_rate),
                "end_timecode": samples_to_timecode(item.end, sample_rate),
                "duration_samples": item.duration,
                "frame_start": item.frame_start,
                "frame_end": item.frame_end,
                "minimum_duration_samples": item.minimum_duration,
                "text_length": item.text_length,
                "max_reading_rate_cps": item.max_reading_rate_cps,
                "estimated_reading_rate_cps": item.estimated_reading_rate_cps,
                "readable": item.readable,
                "status": item.status,
                "decision": item.decision,
                "conflict_objects": sorted(
                    {
                        conflict.get("other_id", "")
                        for conflict in item.conflicts
                        if conflict.get("other_id")
                    }
                ),
            }
        )
    return track


def samples_to_timecode(sample: int, sample_rate: int) -> str:
    millis = (sample * 1000 + sample_rate // 2) // sample_rate
    hours, rem = divmod(millis, 3_600_000)
    minutes, rem = divmod(rem, 60_000)
    seconds, millis = divmod(rem, 1000)
    return f"{hours:02d}:{minutes:02d}:{seconds:02d}.{millis:03d}"

