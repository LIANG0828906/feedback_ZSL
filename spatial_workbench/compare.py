"""Exact three-path comparison and first-difference localization."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

import numpy as np

from .errors import ErrorCode, WorkbenchError
from .models import FrameParameters, RenderResult


@dataclass(frozen=True)
class DiffReport:
    equal: bool
    kind: str = "none"
    first_sample: int | None = None
    first_channel: int | None = None
    first_frame: int | None = None
    detail: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "kind": self.kind,
            "first_sample": self.first_sample,
            "first_channel": self.first_channel,
            "first_frame": self.first_frame,
            "detail": self.detail,
        }


def _frame_for_sample(frames: Iterable[FrameParameters], sample_index: int) -> int:
    ordered = sorted(frames, key=lambda item: item.frame_index)
    if not ordered:
        return 0
    selected = ordered[0].frame_index
    for frame in ordered:
        if frame.dry_start <= sample_index:
            selected = frame.frame_index
        else:
            break
    return selected


def _parameter_difference(
    reference: RenderResult,
    candidate: RenderResult,
) -> DiffReport | None:
    reference_frames = {frame.frame_index: frame for frame in reference.frames}
    candidate_frames = {frame.frame_index: frame for frame in candidate.frames}
    all_indices = sorted(set(reference_frames) | set(candidate_frames))
    for frame_index in all_indices:
        left = reference_frames.get(frame_index)
        right = candidate_frames.get(frame_index)
        if left is None or right is None:
            return DiffReport(
                equal=False,
                kind="frame-set",
                first_frame=frame_index,
                detail="one render has a missing frame",
            )
        if left.fingerprint() != right.fingerprint():
            return DiffReport(
                equal=False,
                kind="parameters",
                first_frame=frame_index,
                detail="reconstruction parameters differ",
            )
    left_stats = reference.render_stats
    right_stats = candidate.render_stats
    for frame_index in sorted(set(left_stats) | set(right_stats)):
        if left_stats.get(frame_index) != right_stats.get(frame_index):
            return DiffReport(
                equal=False,
                kind="energy-statistics",
                first_frame=frame_index,
                detail="per-frame render statistics differ",
            )
    return None


def compare_render_results(
    reference: RenderResult,
    candidate: RenderResult,
) -> DiffReport:
    if reference.channels != candidate.channels:
        return DiffReport(
            equal=False,
            kind="channel-layout",
            first_sample=0,
            first_frame=0,
            detail=f"channel order differs: {reference.channels} != {candidate.channels}",
        )
    if reference.output.shape != candidate.output.shape:
        sample = min(reference.output.shape[-1], candidate.output.shape[-1])
        frame = _frame_for_sample(reference.frames, sample)
        return DiffReport(
            equal=False,
            kind="shape",
            first_sample=int(sample),
            first_frame=frame,
            detail=f"output shapes differ: {reference.output.shape} != {candidate.output.shape}",
        )
    difference = reference.output != candidate.output
    if np.any(difference):
        channel, sample = np.argwhere(difference)[0]
        frame = _frame_for_sample(reference.frames, int(sample))
        return DiffReport(
            equal=False,
            kind="output",
            first_sample=int(sample),
            first_channel=int(channel),
            first_frame=int(frame),
            detail=(
                f"reference={reference.output[channel, sample]!r}, "
                f"candidate={candidate.output[channel, sample]!r}"
            ),
        )
    parameter_diff = _parameter_difference(reference, candidate)
    if parameter_diff is not None:
        return parameter_diff
    return DiffReport(equal=True)


def require_equal(
    reference: RenderResult,
    candidate: RenderResult,
    *,
    reference_path: str,
    candidate_path: str,
) -> None:
    report = compare_render_results(reference, candidate)
    if report.equal:
        return
    raise WorkbenchError(
        ErrorCode.E_DIFF,
        "three-path consistency check failed",
        stage="verification",
        frame_index=report.first_frame,
        channel_index=report.first_channel,
        details={
            "reference_path": reference_path,
            "candidate_path": candidate_path,
            "first_difference": report.to_dict(),
        },
    )


def compare_three_paths(
    realtime: RenderResult,
    recovered: RenderResult,
    replayed: RenderResult,
) -> DiffReport:
    checks = (
        ("realtime", realtime, "recovered", recovered),
        ("realtime", realtime, "replayed", replayed),
        ("recovered", recovered, "replayed", replayed),
    )
    for left_name, left, right_name, right in checks:
        report = compare_render_results(left, right)
        if not report.equal:
            return DiffReport(
                equal=False,
                kind=report.kind,
                first_sample=report.first_sample,
                first_channel=report.first_channel,
                first_frame=report.first_frame,
                detail=f"{left_name} vs {right_name}: {report.detail}",
            )
    return DiffReport(equal=True)


def verify_three_paths(
    realtime: RenderResult,
    recovered: RenderResult,
    replayed: RenderResult,
) -> DiffReport:
    """Return the comparison report or raise E_DIFF with localization."""

    report = compare_three_paths(realtime, recovered, replayed)
    if report.equal:
        return report
    raise WorkbenchError(
        ErrorCode.E_DIFF,
        "three-path consistency check failed",
        stage="verification",
        frame_index=report.first_frame,
        channel_index=report.first_channel,
        details={"first_difference": report.to_dict()},
    )

