from __future__ import annotations

from dataclasses import dataclass, field as dc_field
from typing import Any

from .errors import DiffError
from .models import FrameResult, InputResult, RunResult


@dataclass
class ComparisonReport:
    ok: bool
    code: str | None = None
    component: str = ""
    index: int | None = None
    field: str = ""
    phase: str = ""
    left: Any = None
    right: Any = None
    left_label: str = ""
    right_label: str = ""
    details: dict[str, Any] = dc_field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "code": self.code,
            "component": self.component,
            "index": self.index,
            "field": self.field,
            "phase": self.phase,
            "left": self.left,
            "right": self.right,
            "left_label": self.left_label,
            "right_label": self.right_label,
            "details": self.details,
        }


def _diff(
    left: RunResult,
    right: RunResult,
    component: str,
    index: int | None,
    field_name: str,
    phase: str,
    left_value: Any,
    right_value: Any,
    left_label: str,
    right_label: str,
) -> ComparisonReport:
    return ComparisonReport(
        ok=False,
        code="E_DIFF",
        component=component,
        index=index,
        field=field_name,
        phase=phase,
        left=left_value,
        right=right_value,
        left_label=left_label,
        right_label=right_label,
    )


_FRAME_FIELDS = (
    "timestamp_ms",
    "frame_sha256",
    "wire_sha256",
    "status",
    "committed",
    "codec",
    "width",
    "height",
    "packets_sent",
    "packets_received",
    "fragments_received",
    "retransmissions",
    "fragment_order",
    "fragment_status",
    "missing_fragments",
    "codes",
    "phase",
)
_INPUT_FIELDS = (
    "timestamp_ms",
    "desktop_digest",
    "status",
    "codes",
    "mapped_coords",
    "clip_trace",
    "phase",
)
_STAT_FIELDS = (
    "frames_sent",
    "frames_reassembled",
    "frames_committed",
    "frames_missing",
    "frames_rejected",
    "fragments_sent",
    "fragments_received",
    "fragments_lost",
    "packets_sent",
    "packets_received",
    "packets_corrupt",
    "packets_session_rejected",
    "retransmission_requests",
    "retransmission_recovered",
    "order_events",
    "inputs_total",
    "inputs_replayed",
    "inputs_duplicate",
    "inputs_rejected",
    "phase_rejections",
    "capability_changes_rejected",
    "frames_failed",
    "frame_retries",
)


def compare_runs(
    left: RunResult,
    right: RunResult,
    left_label: str = "left",
    right_label: str = "right",
) -> ComparisonReport:
    if left.content_hash != right.content_hash:
        return _diff(
            left,
            right,
            "session",
            None,
            "content_hash",
            left.final_phase,
            left.content_hash,
            right.content_hash,
            left_label,
            right_label,
        )
    if left.capabilities_hash != right.capabilities_hash:
        return _diff(
            left,
            right,
            "session",
            None,
            "capabilities_hash",
            left.final_phase,
            left.capabilities_hash,
            right.capabilities_hash,
            left_label,
            right_label,
        )
    if left.negotiation != right.negotiation:
        return _diff(
            left,
            right,
            "session",
            None,
            "negotiation",
            left.final_phase,
            left.negotiation.to_dict(),
            right.negotiation.to_dict(),
            left_label,
            right_label,
        )
    left_frames: dict[int, FrameResult] = {
        item.frame_seq: item for item in left.frames
    }
    right_frames: dict[int, FrameResult] = {
        item.frame_seq: item for item in right.frames
    }
    frame_indices = sorted(set(left_frames).union(right_frames))
    frame_mismatch: tuple[int, ComparisonReport] | None = None
    for index in frame_indices:
        left_frame = left_frames.get(index)
        right_frame = right_frames.get(index)
        if left_frame is None or right_frame is None:
            mismatch = _diff(
                left,
                right,
                "frame",
                index,
                "frame_seq",
                left_frame.phase if left_frame else (right_frame.phase if right_frame else ""),
                left_frame is not None,
                right_frame is not None,
                left_label,
                right_label,
            )
            timestamp = (
                left_frame.timestamp_ms
                if left_frame is not None
                else right_frame.timestamp_ms if right_frame is not None else 0
            )
            frame_mismatch = (timestamp, mismatch)
            break
        for field_name in _FRAME_FIELDS:
            left_value = getattr(left_frame, field_name)
            right_value = getattr(right_frame, field_name)
            if left_value != right_value:
                mismatch = _diff(
                    left,
                    right,
                    "frame",
                    index,
                    field_name,
                    left_frame.phase,
                    left_value,
                    right_value,
                    left_label,
                    right_label,
                )
                frame_mismatch = (left_frame.timestamp_ms, mismatch)
                break
        if frame_mismatch is not None:
            break
    left_inputs: dict[int, InputResult] = {
        item.input_seq: item for item in left.inputs
    }
    right_inputs: dict[int, InputResult] = {
        item.input_seq: item for item in right.inputs
    }
    input_indices = sorted(set(left_inputs).union(right_inputs))
    input_mismatch: tuple[int, ComparisonReport] | None = None
    for index in input_indices:
        left_input = left_inputs.get(index)
        right_input = right_inputs.get(index)
        if left_input is None or right_input is None:
            mismatch = _diff(
                left,
                right,
                "input",
                index,
                "input_seq",
                left_input.phase if left_input else (right_input.phase if right_input else ""),
                left_input is not None,
                right_input is not None,
                left_label,
                right_label,
            )
            timestamp = (
                left_input.timestamp_ms
                if left_input is not None
                else right_input.timestamp_ms if right_input is not None else 0
            )
            input_mismatch = (timestamp, mismatch)
            break
        for field_name in _INPUT_FIELDS:
            left_value = getattr(left_input, field_name)
            right_value = getattr(right_input, field_name)
            if left_value != right_value:
                mismatch = _diff(
                    left,
                    right,
                    "input",
                    index,
                    field_name,
                    left_input.phase,
                    left_value,
                    right_value,
                    left_label,
                    right_label,
                )
                input_mismatch = (left_input.timestamp_ms, mismatch)
                break
        if input_mismatch is not None:
            break
    candidates: list[tuple[int, int, ComparisonReport]] = []
    if frame_mismatch is not None:
        candidates.append((frame_mismatch[0], 0, frame_mismatch[1]))
    if input_mismatch is not None:
        candidates.append((input_mismatch[0], 1, input_mismatch[1]))
    if candidates:
        return min(candidates, key=lambda item: (item[0], item[1]))[2]
    if left.final_phase != right.final_phase:
        return _diff(
            left,
            right,
            "session",
            None,
            "final_phase",
            left.final_phase,
            left.final_phase,
            right.final_phase,
            left_label,
            right_label,
        )
    left_desktop = left.desktop_state.get("revision")
    right_desktop = right.desktop_state.get("revision")
    if left.desktop_state != right.desktop_state:
        return _diff(
            left,
            right,
            "desktop",
            None,
            "desktop_state",
            left.final_phase,
            left_desktop,
            right_desktop,
            left_label,
            right_label,
        )
    if len(left.phase_events) != len(right.phase_events):
        return _diff(
            left,
            right,
            "phase",
            min(len(left.phase_events), len(right.phase_events)),
            "phase_event_count",
            left.final_phase,
            len(left.phase_events),
            len(right.phase_events),
            left_label,
            right_label,
        )
    for index, (left_phase, right_phase) in enumerate(
        zip(left.phase_events, right.phase_events, strict=True)
    ):
        for field_name in ("action", "from_phase", "to_phase", "ok", "code"):
            left_value = getattr(left_phase, field_name)
            right_value = getattr(right_phase, field_name)
            if left_value != right_value:
                return _diff(
                    left,
                    right,
                    "phase",
                    index,
                    field_name,
                    left_phase.from_phase,
                    left_value,
                    right_value,
                    left_label,
                    right_label,
                )
    for field_name in _STAT_FIELDS:
        left_value = getattr(left.stats, field_name)
        right_value = getattr(right.stats, field_name)
        if left_value != right_value:
            return _diff(
                left,
                right,
                "session",
                None,
                field_name,
                left.final_phase,
                left_value,
                right_value,
                left_label,
                right_label,
            )
    return ComparisonReport(ok=True)


def compare_paths(
    realtime: RunResult,
    recovery: RunResult,
    replay: RunResult,
) -> ComparisonReport:
    report = compare_runs(realtime, recovery, "realtime", "recovery")
    if not report.ok:
        return report
    return compare_runs(realtime, replay, "realtime", "replay")


def assert_paths_consistent(
    realtime: RunResult,
    recovery: RunResult,
    replay: RunResult,
) -> None:
    report = compare_paths(realtime, recovery, replay)
    if report.ok:
        return
    raise DiffError(
        component=report.component,
        index=report.index,
        field_name=report.field,
        phase=report.phase,
        left=report.left,
        right=report.right,
        left_label=report.left_label,
        right_label=report.right_label,
    )
