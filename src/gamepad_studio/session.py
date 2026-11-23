"""Device reconnect and layout hot-switch migration boundaries."""

from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Any

from .compare import compare_results
from .errors import MappingError
from .hashing import content_hash
from .models import BindingTable, DeviceSpec, SampleBundle
from .pipeline import Pipeline
from .replay import Frame, ReplayResult


@dataclass(frozen=True)
class UnmappedInput:
    device_id: str
    input_id: str
    kind: str
    reason: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "device_id": self.device_id,
            "input_id": self.input_id,
            "kind": self.kind,
            "reason": self.reason,
        }


@dataclass(frozen=True)
class MappingConflict:
    source_device: str
    target_device: str
    axis_id: str
    input_id: str
    input_kind: str
    reason: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "source_device": self.source_device,
            "target_device": self.target_device,
            "axis_id": self.axis_id,
            "input_id": self.input_id,
            "input_kind": self.input_kind,
            "reason": self.reason,
        }


@dataclass(frozen=True)
class SwitchAnalysis:
    source_device: str
    target_device: str
    unmapped: tuple[UnmappedInput, ...]
    conflicts: tuple[MappingConflict, ...]
    migration_notes: tuple[str, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "source_device": self.source_device,
            "target_device": self.target_device,
            "unmapped": [item.to_dict() for item in self.unmapped],
            "conflicts": [item.to_dict() for item in self.conflicts],
            "migration_notes": list(self.migration_notes),
        }


@dataclass(frozen=True)
class HotSwitchResult:
    analysis: SwitchAnalysis
    switch_t_ms: int
    prefix_frames: tuple[Frame, ...]
    suffix_frames: tuple[Frame, ...]
    prefix_hash: str
    suffix_hash: str
    direct_match: bool
    direct_diff: dict[str, Any]
    target_binding_version: str

    @property
    def combined_frames(self) -> tuple[Frame, ...]:
        combined: list[Frame] = []
        for index, frame in enumerate(self.prefix_frames + self.suffix_frames):
            combined.append(replace(frame, frame_index=index))
        return tuple(combined)

    @property
    def combined_hash(self) -> str:
        return content_hash([frame.to_dict() for frame in self.combined_frames])

    def verify_prefix(self) -> bool:
        return content_hash([frame.to_dict() for frame in self.prefix_frames]) == self.prefix_hash

    def to_dict(self) -> dict[str, Any]:
        return {
            "analysis": self.analysis.to_dict(),
            "switch_t_ms": self.switch_t_ms,
            "prefix_frame_count": len(self.prefix_frames),
            "suffix_frame_count": len(self.suffix_frames),
            "prefix_hash": self.prefix_hash,
            "suffix_hash": self.suffix_hash,
            "target_binding_version": self.target_binding_version,
            "direct_match": self.direct_match,
            "direct_diff": self.direct_diff,
            "combined_hash": self.combined_hash,
        }


class HotSwitchSession:
    """A committed source prefix plus an independently replayable target pipeline."""

    def __init__(
        self,
        source_bundle: SampleBundle,
        target_bundle: SampleBundle,
        *,
        switch_t_ms: int,
        target_bindings: BindingTable | None = None,
        max_event_buffer: int = 256,
        window_size: int = 32,
    ) -> None:
        self.source_bundle = source_bundle
        self.target_bundle = replace(
            target_bundle,
            bindings=target_bindings or target_bundle.bindings,
        )
        self.switch_t_ms = switch_t_ms
        self.max_event_buffer = max_event_buffer
        self.window_size = window_size
        self.source_pipeline = Pipeline(
            source_bundle,
            max_event_buffer=max_event_buffer,
            window_size=window_size,
        )
        self.target_pipeline = Pipeline(
            self.target_bundle,
            max_event_buffer=max_event_buffer,
            window_size=window_size,
        )
        self.source_run = self.source_pipeline.recompute()
        self.target_run = self.target_pipeline.recompute()
        self.result = self._build_result()

    @staticmethod
    def analyze(
        source_bundle: SampleBundle,
        target_bundle: SampleBundle,
        *,
        target_bindings: BindingTable | None = None,
    ) -> SwitchAnalysis:
        bindings = target_bindings or target_bundle.bindings
        source_device = source_bundle.device
        target_device = target_bundle.device
        conflicts: list[MappingConflict] = []
        target_axes = set(target_device.axis_ids)
        target_buttons = set(target_device.buttons)
        referenced_axes = {
            rule.source_axis
            for rule in bindings.rules
            if rule.kind == "axis" and rule.source_axis is not None
        }
        referenced_buttons = {
            button for rule in bindings.rules for button in rule.buttons
        }
        for index, rule in enumerate(bindings.rules):
            if rule.kind == "axis" and rule.source_axis and rule.source_axis not in target_axes:
                conflicts.append(
                    MappingConflict(
                        source_device=source_device.id,
                        target_device=target_device.id,
                        axis_id=rule.source_axis,
                        input_id=rule.source_axis,
                        input_kind="axis",
                        reason=f"binding {rule.id!r} references axis absent on target",
                    )
                )
            for button in rule.buttons:
                if button not in target_buttons:
                    conflicts.append(
                        MappingConflict(
                            source_device=source_device.id,
                            target_device=target_device.id,
                            axis_id="",
                            input_id=button,
                            input_kind="button",
                            reason=f"binding {rule.id!r} references button absent on target",
                        )
                    )
        unmapped: list[UnmappedInput] = []
        source_axes = set(source_device.axis_ids)
        source_buttons = set(source_device.buttons)
        for axis_id in sorted(source_axes - target_axes):
            unmapped.append(
                UnmappedInput(
                    device_id=source_device.id,
                    input_id=axis_id,
                    kind="axis",
                    reason="axis not present on target; committed source results are retained",
                )
            )
        for button in sorted(source_buttons - target_buttons):
            unmapped.append(
                UnmappedInput(
                    device_id=source_device.id,
                    input_id=button,
                    kind="button",
                    reason="button not present on target; committed source results are retained",
                )
            )
        for axis_id in sorted(target_axes - referenced_axes):
            unmapped.append(
                UnmappedInput(
                    device_id=target_device.id,
                    input_id=axis_id,
                    kind="axis",
                    reason="target axis has no binding rule",
                )
            )
        for button in sorted(target_buttons - referenced_buttons):
            unmapped.append(
                UnmappedInput(
                    device_id=target_device.id,
                    input_id=button,
                    kind="button",
                    reason="target button has no binding rule",
                )
            )
        notes: list[str] = []
        source_axis_map = source_device.axis_map
        target_axis_map = target_device.axis_map
        for axis_id in sorted(source_axes & target_axes):
            source_axis = source_axis_map[axis_id]
            target_axis = target_axis_map[axis_id]
            if source_axis.kind != target_axis.kind:
                notes.append(
                    f"axis {axis_id}: kind changed {source_axis.kind} -> {target_axis.kind}"
                )
            if source_axis.trigger_direction != target_axis.trigger_direction:
                notes.append(
                    f"axis {axis_id}: trigger direction changed "
                    f"{source_axis.trigger_direction} -> {target_axis.trigger_direction}"
                )
            if (source_axis.minimum, source_axis.maximum) != (
                target_axis.minimum,
                target_axis.maximum,
            ):
                notes.append(
                    f"axis {axis_id}: range changed "
                    f"[{source_axis.minimum}, {source_axis.maximum}] -> "
                    f"[{target_axis.minimum}, {target_axis.maximum}]"
                )
            if source_axis.deadzone != target_axis.deadzone:
                notes.append(
                    f"axis {axis_id}: deadzone changed "
                    f"{source_axis.deadzone} -> {target_axis.deadzone}"
                )
        if len(target_device.axes) != len(source_device.axes):
            notes.append(
                f"axis count changed {len(source_device.axes)} -> {len(target_device.axes)}"
            )
        if len(target_device.buttons) != len(source_device.buttons):
            notes.append(
                f"button count changed {len(source_device.buttons)} -> "
                f"{len(target_device.buttons)}"
            )
        return SwitchAnalysis(
            source_device=source_device.id,
            target_device=target_device.id,
            unmapped=tuple(unmapped),
            conflicts=tuple(conflicts),
            migration_notes=tuple(notes),
        )

    @staticmethod
    def raise_for_conflicts(analysis: SwitchAnalysis) -> None:
        if not analysis.conflicts:
            return
        first = analysis.conflicts[0]
        identifiers = ", ".join(
            conflict.axis_id or conflict.input_id for conflict in analysis.conflicts
        )
        raise MappingError(
            f"cannot migrate {analysis.source_device!r} to "
            f"{analysis.target_device!r}: mapping conflicts at {identifiers}",
            field="axis" if first.axis_id else "input",
            context={
                "source_device": analysis.source_device,
                "target_device": analysis.target_device,
                "axis_id": first.axis_id,
                "conflicts": [conflict.to_dict() for conflict in analysis.conflicts],
            },
        )

    def _frames_before_or_at(self, result: ReplayResult) -> tuple[Frame, ...]:
        return tuple(frame for frame in result.frames if frame.t_ms <= self.switch_t_ms)

    def _frames_after(self, result: ReplayResult) -> tuple[Frame, ...]:
        return tuple(frame for frame in result.frames if frame.t_ms > self.switch_t_ms)

    def _build_result(self) -> HotSwitchResult:
        analysis = self.analyze(
            self.source_bundle,
            self.target_bundle,
            target_bindings=self.target_bundle.bindings,
        )
        self.raise_for_conflicts(analysis)
        prefix = self._frames_before_or_at(self.source_run.result)
        suffix = self._frames_after(self.target_run.result)
        direct_pipeline = Pipeline(
            self.target_bundle,
            max_event_buffer=self.max_event_buffer,
            window_size=self.window_size,
        )
        direct_run = direct_pipeline.recompute()
        direct_frames = self._frames_after(direct_run.result)
        direct_equal = suffix == direct_frames
        direct_diff = {
            "equal": direct_equal,
            "checked_frames": len(direct_frames),
            "message": (
                "post-switch target stream equals direct target replay"
                if direct_equal
                else "post-switch target stream differs from direct target replay"
            ),
        }
        return HotSwitchResult(
            analysis=analysis,
            switch_t_ms=self.switch_t_ms,
            prefix_frames=prefix,
            suffix_frames=suffix,
            prefix_hash=content_hash([frame.to_dict() for frame in prefix]),
            suffix_hash=content_hash([frame.to_dict() for frame in suffix]),
            direct_match=direct_equal,
            direct_diff=direct_diff,
            target_binding_version=self.target_bundle.binding_version,
        )

    def verify_direct_target(self) -> dict[str, Any]:
        direct = Pipeline(
            self.target_bundle,
            max_event_buffer=self.max_event_buffer,
            window_size=self.window_size,
        ).recompute()
        suffix = self._frames_after(self.target_run.result)
        direct_frames = self._frames_after(direct.result)
        suffix_result = ReplayResult(
            frames=suffix,
            input_hash=self.target_bundle.input_hash,
            binding_version=self.target_bundle.binding_version,
            algorithm_version=self.target_run.result.algorithm_version,
            axis_ids=self.target_bundle.device.axis_ids,
            action_ids=self.target_run.result.action_ids,
        )
        direct_result = ReplayResult(
            frames=direct_frames,
            input_hash=self.target_bundle.input_hash,
            binding_version=self.target_bundle.binding_version,
            algorithm_version=direct.result.algorithm_version,
            axis_ids=self.target_bundle.device.axis_ids,
            action_ids=direct.result.action_ids,
        )
        report = compare_results(suffix_result, direct_result)
        return report.to_dict()

    def update_target(self, *, device: DeviceSpec | None = None, bindings: BindingTable | None = None) -> Any:
        if not self.result.verify_prefix():
            raise MappingError(
                "committed source prefix was mutated",
                field="prefix",
                context={"prefix_hash": self.result.prefix_hash},
            )
        return self.target_pipeline.update(device=device, bindings=bindings)


def plan_hot_switch(
    source_bundle: SampleBundle,
    target_bundle: SampleBundle,
    *,
    target_bindings: BindingTable | None = None,
) -> SwitchAnalysis:
    return HotSwitchSession.analyze(
        source_bundle,
        target_bundle,
        target_bindings=target_bindings,
    )


__all__ = [
    "HotSwitchResult",
    "HotSwitchSession",
    "MappingConflict",
    "SwitchAnalysis",
    "UnmappedInput",
    "plan_hot_switch",
]
