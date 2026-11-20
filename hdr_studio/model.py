from __future__ import annotations

import math
from collections.abc import Iterable
from dataclasses import dataclass, replace
from enum import StrEnum
from itertools import pairwise
from typing import Any

from .util import canonical_bytes, sha256_hex


class ColorSpace(StrEnum):
    REC709 = "rec709"
    REC2020 = "rec2020"
    DISPLAY_P3 = "display_p3"


class TransferFunction(StrEnum):
    SDR_GAMMA = "sdr_gamma"
    PQ = "pq"
    HLG = "hlg"


class ToneMap(StrEnum):
    NONE = "none"
    REINHARD = "reinhard"
    HABLE = "hable"


COLOR_SPACE_CODES: dict[int, ColorSpace] = {
    1: ColorSpace.REC709,
    2: ColorSpace.REC2020,
    3: ColorSpace.DISPLAY_P3,
}
COLOR_SPACE_TO_CODE = {value: key for key, value in COLOR_SPACE_CODES.items()}

TRANSFER_CODES: dict[int, TransferFunction] = {
    1: TransferFunction.SDR_GAMMA,
    2: TransferFunction.PQ,
    3: TransferFunction.HLG,
}
TRANSFER_TO_CODE = {value: key for key, value in TRANSFER_CODES.items()}


@dataclass(frozen=True, slots=True)
class ExposureSegment:
    start: int
    end: int
    ev: float

    def __post_init__(self) -> None:
        if self.start < 0 or self.end < self.start:
            raise ValueError("exposure segment must satisfy 0 <= start <= end")
        if not math.isfinite(self.ev):
            raise ValueError("exposure EV must be finite")

    def contains(self, frame_index: int) -> bool:
        return self.start <= frame_index <= self.end

    def to_dict(self) -> dict[str, Any]:
        return {
            "start": self.start,
            "end": self.end,
            "ev": 0.0 if self.ev == 0.0 else self.ev,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> ExposureSegment:
        return cls(start=int(value["start"]), end=int(value["end"]), ev=float(value["ev"]))


@dataclass(frozen=True, slots=True)
class ConversionParams:
    target_color_space: ColorSpace
    target_transfer: TransferFunction
    peak_luminance: float
    tone_map: ToneMap = ToneMap.NONE
    exposure_segments: tuple[ExposureSegment, ...] = ()

    def __post_init__(self) -> None:
        if not math.isfinite(self.peak_luminance) or self.peak_luminance <= 0:
            raise ValueError("peak luminance must be finite and greater than zero")

    def normalized(self, frame_count: int | None = None) -> ConversionParams:
        segments = sorted(self.exposure_segments, key=lambda item: (item.start, item.end))
        if frame_count is not None:
            for segment in segments:
                if segment.end >= frame_count:
                    raise ValueError(
                        f"exposure segment [{segment.start}, {segment.end}] is outside "
                        f"0..{frame_count - 1}"
                    )
        for previous, current in pairwise(segments):
            if current.start <= previous.end:
                raise ValueError("exposure segments must not overlap")
        return replace(self, exposure_segments=tuple(segments))

    def ev_for_frame(self, frame_index: int) -> float:
        # Sorted non-overlapping segments make this deterministic.
        for segment in self.exposure_segments:
            if segment.contains(frame_index):
                return segment.ev
        return 0.0

    def ev_vector(self, frame_count: int) -> list[float]:
        values = [0.0] * frame_count
        for segment in self.exposure_segments:
            for index in range(segment.start, min(segment.end, frame_count - 1) + 1):
                values[index] = segment.ev
        return values

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_color_space": self.target_color_space.value,
            "target_transfer": self.target_transfer.value,
            "peak_luminance": self.peak_luminance,
            "tone_map": self.tone_map.value,
            "exposure_segments": [segment.to_dict() for segment in self.exposure_segments],
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> ConversionParams:
        return cls(
            target_color_space=ColorSpace(value["target_color_space"]),
            target_transfer=TransferFunction(value["target_transfer"]),
            peak_luminance=float(value["peak_luminance"]),
            tone_map=ToneMap(value["tone_map"]),
            exposure_segments=tuple(
                ExposureSegment.from_dict(segment)
                for segment in value.get("exposure_segments", [])
            ),
        ).normalized()


@dataclass(frozen=True, slots=True)
class PreviewState:
    selected_version: int | None = None
    frame_index: int = 0
    playing: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "selected_version": self.selected_version,
            "frame_index": self.frame_index,
            "playing": self.playing,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any] | None) -> PreviewState:
        if value is None:
            return cls()
        return cls(
            selected_version=(
                None if value.get("selected_version") is None else int(value["selected_version"])
            ),
            frame_index=int(value.get("frame_index", 0)),
            playing=bool(value.get("playing", False)),
        )


@dataclass(frozen=True, slots=True)
class DatasetInfo:
    relative_path: str
    file_hash: str
    header_hash: str
    width: int
    height: int
    frame_count: int
    bit_depth: int
    color_space: ColorSpace
    transfer: TransferFunction
    peak_luminance: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "relative_path": self.relative_path,
            "file_hash": self.file_hash,
            "header_hash": self.header_hash,
            "width": self.width,
            "height": self.height,
            "frame_count": self.frame_count,
            "bit_depth": self.bit_depth,
            "color_space": self.color_space.value,
            "transfer": self.transfer.value,
            "peak_luminance": self.peak_luminance,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> DatasetInfo:
        return cls(
            relative_path=str(value["relative_path"]),
            file_hash=str(value["file_hash"]),
            header_hash=str(value["header_hash"]),
            width=int(value["width"]),
            height=int(value["height"]),
            frame_count=int(value["frame_count"]),
            bit_depth=int(value["bit_depth"]),
            color_space=ColorSpace(value["color_space"]),
            transfer=TransferFunction(value["transfer"]),
            peak_luminance=float(value["peak_luminance"]),
        )


@dataclass(frozen=True, slots=True)
class VersionRecord:
    version: int
    parent_version: int | None
    parent_record_hash: str | None
    operation: str
    params: ConversionParams
    affected_intervals: tuple[tuple[int, int], ...]
    affected_count: int
    recomputed_count: int
    reused_count: int
    frame_hashes: tuple[str, ...]
    output_hash: str
    intermediate_summary: dict[str, Any]
    frame_summaries: tuple[dict[str, Any], ...]
    rollback_target: int | None = None
    record_hash: str = ""

    def payload(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "parent_version": self.parent_version,
            "parent_record_hash": self.parent_record_hash,
            "operation": self.operation,
            "params": self.params.to_dict(),
            "affected_intervals": [list(item) for item in self.affected_intervals],
            "affected_count": self.affected_count,
            "recomputed_count": self.recomputed_count,
            "reused_count": self.reused_count,
            "frame_hashes": list(self.frame_hashes),
            "output_hash": self.output_hash,
            "intermediate_summary": self.intermediate_summary,
            "frame_summaries": list(self.frame_summaries),
            "rollback_target": self.rollback_target,
        }

    def to_dict(self) -> dict[str, Any]:
        result = self.payload()
        result["record_hash"] = self.record_hash
        return result

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> VersionRecord:
        return cls(
            version=int(value["version"]),
            parent_version=(
                None if value.get("parent_version") is None else int(value["parent_version"])
            ),
            parent_record_hash=value.get("parent_record_hash"),
            operation=str(value["operation"]),
            params=ConversionParams.from_dict(value["params"]),
            affected_intervals=tuple(
                (int(item[0]), int(item[1])) for item in value["affected_intervals"]
            ),
            affected_count=int(value["affected_count"]),
            recomputed_count=int(value["recomputed_count"]),
            reused_count=int(value["reused_count"]),
            frame_hashes=tuple(str(item) for item in value["frame_hashes"]),
            output_hash=str(value["output_hash"]),
            intermediate_summary=dict(value["intermediate_summary"]),
            frame_summaries=tuple(dict(item) for item in value["frame_summaries"]),
            rollback_target=(
                None
                if value.get("rollback_target") is None
                else int(value["rollback_target"])
            ),
            record_hash=str(value["record_hash"]),
        )

    def compute_hash(self) -> str:
        return sha256_hex(canonical_bytes(self.payload()))

    def with_computed_hash(self) -> VersionRecord:
        return replace(self, record_hash=self.compute_hash())

    def verify_hash(self) -> bool:
        return bool(self.record_hash) and self.record_hash == self.compute_hash()


@dataclass(frozen=True, slots=True)
class VersionDiff:
    left_version: int
    right_version: int
    changed_params: tuple[str, ...]
    changed_frames: tuple[int, ...]
    left_output_hash: str
    right_output_hash: str
    same_content: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "left_version": self.left_version,
            "right_version": self.right_version,
            "changed_params": list(self.changed_params),
            "changed_frames": list(self.changed_frames),
            "left_output_hash": self.left_output_hash,
            "right_output_hash": self.right_output_hash,
            "same_content": self.same_content,
        }


def compare_param_dicts(
    left: ConversionParams, right: ConversionParams
) -> tuple[str, ...]:
    left_dict = left.to_dict()
    right_dict = right.to_dict()
    changed: list[str] = []
    for key in left_dict:
        if left_dict[key] != right_dict[key]:
            changed.append(key)
    return tuple(changed)


def changed_frame_indices(
    left: ConversionParams,
    right: ConversionParams,
    frame_count: int,
    *,
    full_recompute: bool = False,
) -> list[int]:
    if full_recompute:
        return list(range(frame_count))
    left_ev = left.ev_vector(frame_count)
    right_ev = right.ev_vector(frame_count)
    return [
        index
        for index, (left_value, right_value) in enumerate(zip(left_ev, right_ev))
        if left_value != right_value
    ]


def frames_in_intervals(intervals: Iterable[Iterable[int]]) -> list[int]:
    result: set[int] = set()
    for start, end in intervals:
        result.update(range(int(start), int(end) + 1))
    return sorted(result)
