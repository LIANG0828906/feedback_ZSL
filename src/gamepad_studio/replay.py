"""Deterministic logical-time replay and action timeline assembly."""

from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Any, Callable, Iterable, Iterator, Sequence

from .curves import EPSILON, apply_binding_curve, normalize_axis
from .errors import ALGORITHM_VERSION
from .hashing import content_hash
from .models import (
    ActionSnapshot,
    AxisFrame,
    AxisSpec,
    BindingRule,
    InputEvent,
    SampleBundle,
    TruthManifest,
)


def iter_event_chunks(events: Sequence[InputEvent], max_events: int) -> Iterator[Sequence[InputEvent]]:
    if max_events <= 0:
        raise ValueError("max_events must be positive")
    for start in range(0, len(events), max_events):
        yield events[start : start + max_events]


@dataclass(frozen=True, order=True)
class TickKey:
    t_ms: int
    phase: int
    order: int
    tag: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "t_ms": self.t_ms,
            "phase": self.phase,
            "order": self.order,
            "tag": self.tag,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "TickKey":
        return cls(
            t_ms=int(data["t_ms"]),
            phase=int(data["phase"]),
            order=int(data["order"]),
            tag=str(data.get("tag", "")),
        )


@dataclass(frozen=True)
class RuleTick:
    rule_id: str
    action: str
    key: TickKey
    source_event_seq: int
    kind: str
    state: ActionSnapshot

    def to_dict(self) -> dict[str, Any]:
        return {
            "rule_id": self.rule_id,
            "action": self.action,
            "key": self.key.to_dict(),
            "source_event_seq": self.source_event_seq,
            "kind": self.kind,
            "state": self.state.to_dict(),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "RuleTick":
        return cls(
            rule_id=str(data["rule_id"]),
            action=str(data["action"]),
            key=TickKey.from_dict(data["key"]),
            source_event_seq=int(data["source_event_seq"]),
            kind=str(data["kind"]),
            state=ActionSnapshot.from_dict(data["state"]),
        )


@dataclass(frozen=True)
class RuleTimeline:
    rule_id: str
    action: str
    ticks: tuple[RuleTick, ...]
    final_state: ActionSnapshot
    window_ids: tuple[str, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "rule_id": self.rule_id,
            "action": self.action,
            "ticks": [tick.to_dict() for tick in self.ticks],
            "final_state": self.final_state.to_dict(),
            "window_ids": list(self.window_ids),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "RuleTimeline":
        return cls(
            rule_id=str(data["rule_id"]),
            action=str(data["action"]),
            ticks=tuple(RuleTick.from_dict(item) for item in data["ticks"]),
            final_state=ActionSnapshot.from_dict(data["final_state"]),
            window_ids=tuple(str(item) for item in data.get("window_ids", [])),
        )


@dataclass(frozen=True)
class Frame:
    frame_index: int
    t_ms: int
    source_event_seq: int
    tick_kind: str
    timer_rule: str | None
    tick_key: TickKey
    axis_values: dict[str, float]
    actions: dict[str, ActionSnapshot]

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.frame_index,
            "t_ms": self.t_ms,
            "source_event_seq": self.source_event_seq,
            "tick_kind": self.tick_kind,
            "timer_rule": self.timer_rule,
            "tick_key": self.tick_key.to_dict(),
            "axis_values": dict(sorted(self.axis_values.items())),
            "actions": {
                action: snapshot.to_dict()
                for action, snapshot in sorted(self.actions.items())
            },
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Frame":
        return cls(
            frame_index=int(data["frame_index"]),
            t_ms=int(data["t_ms"]),
            source_event_seq=int(data["source_event_seq"]),
            tick_kind=str(data["tick_kind"]),
            timer_rule=(
                str(data["timer_rule"]) if data.get("timer_rule") is not None else None
            ),
            tick_key=TickKey.from_dict(data["tick_key"]),
            axis_values={
                str(axis): float(value)
                for axis, value in data.get("axis_values", {}).items()
            },
            actions={
                str(action): ActionSnapshot.from_dict(snapshot)
                for action, snapshot in data.get("actions", {}).items()
            },
        )


@dataclass(frozen=True)
class ReplayResult:
    frames: tuple[Frame, ...]
    input_hash: str
    binding_version: str
    algorithm_version: str
    axis_ids: tuple[str, ...]
    action_ids: tuple[str, ...]

    @property
    def result_hash(self) -> str:
        return content_hash(
            {
                "input_hash": self.input_hash,
                "binding_version": self.binding_version,
                "algorithm_version": self.algorithm_version,
                "frames": [frame.to_dict() for frame in self.frames],
            }
        )

    def frame_at(self, t_ms: float, *, tolerance_ms: int = 0) -> Frame | None:
        chosen: Frame | None = None
        for frame in self.frames:
            if frame.t_ms <= t_ms + tolerance_ms:
                if abs(frame.t_ms - t_ms) <= tolerance_ms:
                    chosen = frame
                elif chosen is None or frame.t_ms < t_ms:
                    chosen = frame
            elif frame.t_ms > t_ms + tolerance_ms and chosen is not None:
                break
        return chosen

    def latest_at_or_before(self, t_ms: int) -> Frame | None:
        chosen: Frame | None = None
        for frame in self.frames:
            if frame.t_ms <= t_ms:
                chosen = frame
            else:
                break
        return chosen

    def to_dict(self) -> dict[str, Any]:
        return {
            "frames": [frame.to_dict() for frame in self.frames],
            "input_hash": self.input_hash,
            "binding_version": self.binding_version,
            "algorithm_version": self.algorithm_version,
            "axis_ids": list(self.axis_ids),
            "action_ids": list(self.action_ids),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ReplayResult":
        return cls(
            frames=tuple(Frame.from_dict(item) for item in data["frames"]),
            input_hash=str(data["input_hash"]),
            binding_version=str(data["binding_version"]),
            algorithm_version=str(data["algorithm_version"]),
            axis_ids=tuple(str(item) for item in data.get("axis_ids", [])),
            action_ids=tuple(str(item) for item in data.get("action_ids", [])),
        )


def compute_axis_frames(
    axis: AxisSpec,
    events: Sequence[InputEvent],
    *,
    max_event_buffer: int = 256,
) -> tuple[AxisFrame, ...]:
    frames: list[AxisFrame] = []
    for chunk in iter_event_chunks(events, max_event_buffer):
        for event in chunk:
            if event.type != "axis" or event.axis != axis.id:
                continue
            assert event.value is not None
            oriented, normalized = normalize_axis(axis, event.value)
            frames.append(
                AxisFrame(
                    seq=event.seq,
                    t_ms=event.t_ms,
                    axis=axis.id,
                    raw=event.value,
                    oriented=oriented,
                    normalized=normalized,
                )
            )
    return tuple(frames)


def compute_all_axis_frames(
    bundle: SampleBundle,
    *,
    max_event_buffer: int = 256,
) -> dict[str, tuple[AxisFrame, ...]]:
    return {
        axis.id: compute_axis_frames(
            axis,
            bundle.events,
            max_event_buffer=max_event_buffer,
        )
        for axis in bundle.device.axes
    }


@dataclass
class _MutableRuleState:
    action: str
    pressed: bool = False
    active: bool = False
    value: float = 0.0
    trigger_count: int = 0
    repeat_count: int = 0
    trigger_t_ms: int | None = None
    trigger_seq: int | None = None
    last_reason: str = "idle"

    def snapshot(self, *, triggered_now: bool = False) -> ActionSnapshot:
        return ActionSnapshot(
            action=self.action,
            pressed=self.pressed,
            active=self.active,
            value=self.value,
            trigger_count=self.trigger_count,
            repeat_count=self.repeat_count,
            trigger_t_ms=self.trigger_t_ms,
            trigger_seq=self.trigger_seq,
            last_reason=self.last_reason,
            triggered_now=triggered_now,
        )

    def initial(self) -> ActionSnapshot:
        return self.snapshot(triggered_now=False)


def _relevant_groups(
    events: Sequence[InputEvent],
    predicate: Callable[[InputEvent], bool],
    *,
    max_event_buffer: int,
) -> Iterator[list[InputEvent]]:
    group: list[InputEvent] = []
    current_t: int | None = None
    for chunk in iter_event_chunks(events, max_event_buffer):
        for event in chunk:
            if not predicate(event):
                continue
            if current_t is None:
                current_t = event.t_ms
            if event.t_ms != current_t:
                yield group
                group = []
                current_t = event.t_ms
            group.append(event)
    if group:
        yield group


def _rule_window_ids(rule: BindingRule, source_events: Iterable[InputEvent], window_size: int) -> tuple[str, ...]:
    ids = {f"w{event.seq // window_size}" for event in source_events}
    return tuple(sorted(ids, key=lambda value: int(value[1:])))


def replay_rule(
    rule: BindingRule,
    axis: AxisSpec | None,
    events: Sequence[InputEvent],
    axis_frames: dict[int, AxisFrame],
    *,
    rule_rank: int,
    max_event_buffer: int = 256,
    window_size: int = 32,
) -> RuleTimeline:
    state = _MutableRuleState(action=rule.action)
    ticks: list[RuleTick] = []
    relevant_seqs: list[int] = []

    def emit(
        key: TickKey,
        source_seq: int,
        kind: str,
        *,
        triggered_now: bool = False,
    ) -> None:
        ticks.append(
            RuleTick(
                rule_id=rule.id,
                action=rule.action,
                key=key,
                source_event_seq=source_seq,
                kind=kind,
                state=state.snapshot(triggered_now=triggered_now),
            )
        )

    if not rule.enabled:
        return RuleTimeline(
            rule_id=rule.id,
            action=rule.action,
            ticks=(),
            final_state=state.initial(),
            window_ids=(),
        )

    if rule.kind == "axis":
        if not rule.source_axis or axis is None:
            raise ValueError(f"axis rule {rule.id!r} is missing its source axis")

        def predicate(event: InputEvent) -> bool:
            return event.type == "axis" and event.axis == rule.source_axis

        for group in _relevant_groups(events, predicate, max_event_buffer=max_event_buffer):
            for event in group:
                relevant_seqs.append(event.seq)
                frame = axis_frames[event.seq]
                value = apply_binding_curve(axis, frame.oriented, rule.curve)
                was_active = state.active
                state.value = value
                state.active = abs(value) > EPSILON
                state.pressed = state.active
                triggered = False
                if state.active and not was_active:
                    state.trigger_count += 1
                    state.trigger_t_ms = event.t_ms
                    state.trigger_seq = event.seq
                    state.last_reason = "axis_activated"
                    triggered = True
                elif not state.active and was_active:
                    state.last_reason = "axis_released"
                else:
                    state.last_reason = "axis_update"
                emit(
                    TickKey(event.t_ms, 1, event.seq, ""),
                    event.seq,
                    "event",
                    triggered_now=triggered,
                )
        return RuleTimeline(
            rule_id=rule.id,
            action=rule.action,
            ticks=tuple(ticks),
            final_state=state.initial(),
            window_ids=_rule_window_ids(rule, (event for event in events if event.seq in relevant_seqs), window_size),
        )

    rule_buttons = set(rule.buttons)

    def button_predicate(event: InputEvent) -> bool:
        return event.type == "button" and event.button in rule_buttons

    down: set[str] = set()
    first_press_t: int | None = None
    first_press_seq: int | None = None
    stage = 0
    combo_blocked = False
    pending_deadline_t: int | None = None
    pending_deadline_kind: str | None = None
    pending_source_seq: int | None = None
    long_triggered = False

    def timer_key(t_ms: int, kind: str, source_seq: int) -> TickKey:
        if kind == "long":
            return TickKey(t_ms, 0, source_seq, rule.id)
        return TickKey(t_ms, 2, rule_rank, rule.id)

    def process_timer() -> None:
        nonlocal pending_deadline_t, pending_deadline_kind, pending_source_seq
        nonlocal first_press_t, first_press_seq, stage, long_triggered
        nonlocal combo_blocked
        if pending_deadline_t is None or pending_deadline_kind is None:
            return
        deadline_t = pending_deadline_t
        kind = pending_deadline_kind
        source_seq = pending_source_seq if pending_source_seq is not None else -1
        key = timer_key(deadline_t, kind, source_seq)
        if kind == "long":
            if down and not long_triggered:
                long_triggered = True
                state.pressed = True
                state.active = True
                state.value = 1.0
                state.trigger_count += 1
                state.trigger_t_ms = deadline_t
                state.trigger_seq = source_seq
                state.last_reason = "long_triggered"
                emit(key, source_seq, "long_deadline", triggered_now=True)
        else:
            if rule.kind == "combo":
                if first_press_t is not None and not state.active:
                    first_press_t = None
                    first_press_seq = None
                    combo_blocked = bool(down)
                    state.last_reason = "combo_window_expired"
                    emit(key, source_seq, "window_deadline")
            elif rule.kind == "double_press":
                if stage != 0:
                    first_press_t = None
                    first_press_seq = None
                    stage = 0
                    state.last_reason = "double_window_expired"
                    emit(key, source_seq, "window_deadline")
        pending_deadline_t = None
        pending_deadline_kind = None
        pending_source_seq = None

    def maybe_process_before(t_ms: int) -> None:
        if pending_deadline_t is None or pending_deadline_kind is None:
            return
        if pending_deadline_t < t_ms or (
            pending_deadline_kind == "long" and pending_deadline_t == t_ms
        ):
            process_timer()

    def maybe_process_after(t_ms: int) -> None:
        if (
            pending_deadline_t is not None
            and pending_deadline_kind == "window"
            and pending_deadline_t <= t_ms
        ):
            process_timer()

    for group in _relevant_groups(events, button_predicate, max_event_buffer=max_event_buffer):
        group_t = group[0].t_ms
        maybe_process_before(group_t)
        for event in group:
            assert event.button is not None
            assert event.pressed is not None
            relevant_seqs.append(event.seq)
            key = TickKey(event.t_ms, 1, event.seq, "")
            if rule.kind == "button":
                if event.pressed:
                    if event.repeat and event.button in down:
                        state.repeat_count += 1
                        state.trigger_count += 1
                        state.trigger_t_ms = event.t_ms
                        state.trigger_seq = event.seq
                        state.pressed = True
                        state.active = True
                        state.value = 1.0
                        state.last_reason = "button_auto_repeat"
                        emit(key, event.seq, "event", triggered_now=True)
                    elif event.button not in down:
                        down.add(event.button)
                        state.pressed = True
                        state.active = True
                        state.value = 1.0
                        state.trigger_count += 1
                        state.trigger_t_ms = event.t_ms
                        state.trigger_seq = event.seq
                        state.last_reason = "button_pressed"
                        emit(key, event.seq, "event", triggered_now=True)
                elif event.button in down:
                    down.discard(event.button)
                    state.pressed = False
                    state.active = False
                    state.value = 0.0
                    state.last_reason = "button_released"
                    emit(key, event.seq, "event")
            elif rule.kind == "combo":
                if event.pressed and not event.repeat:
                    down.add(event.button)
                    emitted = False
                    if combo_blocked:
                        state.last_reason = "combo_blocked_until_release"
                        emit(key, event.seq, "event")
                        emitted = True
                    elif first_press_t is None and not state.active:
                        first_press_t = event.t_ms
                        first_press_seq = event.seq
                        pending_deadline_t = event.t_ms + rule.window_ms
                        pending_deadline_kind = "window"
                        pending_source_seq = event.seq
                    if (
                        not state.active
                        and rule_buttons.issubset(down)
                        and first_press_t is not None
                    ):
                        if event.t_ms - first_press_t <= rule.window_ms:
                            state.pressed = True
                            state.active = True
                            state.value = 1.0
                            state.trigger_count += 1
                            state.trigger_t_ms = event.t_ms
                            state.trigger_seq = event.seq
                            state.last_reason = "combo_triggered"
                            first_press_t = None
                            first_press_seq = None
                            pending_deadline_t = None
                            pending_deadline_kind = None
                            pending_source_seq = None
                            emit(key, event.seq, "event", triggered_now=True)
                            emitted = True
                        else:
                            state.last_reason = "combo_window_expired"
                            first_press_t = None
                            first_press_seq = None
                            pending_deadline_t = None
                            pending_deadline_kind = None
                            pending_source_seq = None
                            emit(key, event.seq, "event")
                            emitted = True
                    if not emitted and not state.active:
                        state.last_reason = "combo_awaiting_members"
                        emit(key, event.seq, "event")
                elif not event.pressed:
                    was_down = event.button in down
                    down.discard(event.button)
                    if not down:
                        combo_blocked = False
                    if state.active:
                        state.pressed = False
                        state.active = False
                        state.value = 0.0
                        state.last_reason = "combo_released"
                        combo_blocked = bool(down)
                        first_press_t = None
                        first_press_seq = None
                        emit(key, event.seq, "event")
                    elif was_down and first_press_t is not None:
                        state.last_reason = "combo_cancelled_member_released"
                        combo_blocked = bool(down)
                        first_press_t = None
                        first_press_seq = None
                        pending_deadline_t = None
                        pending_deadline_kind = None
                        pending_source_seq = None
                        emit(key, event.seq, "event")
            elif rule.kind == "long_press":
                if event.pressed and not event.repeat:
                    if event.button not in down:
                        down.add(event.button)
                        long_triggered = False
                        pending_deadline_t = event.t_ms + rule.hold_ms
                        pending_deadline_kind = "long"
                        pending_source_seq = event.seq
                        state.pressed = False
                        state.active = False
                        state.value = 0.0
                        state.last_reason = "long_armed"
                        emit(key, event.seq, "event")
                elif not event.pressed:
                    if event.button in down:
                        down.discard(event.button)
                        pending_deadline_t = None
                        pending_deadline_kind = None
                        pending_source_seq = None
                        state.pressed = False
                        state.active = False
                        state.value = 0.0
                        if long_triggered:
                            state.last_reason = "long_released"
                        else:
                            state.last_reason = "long_released_early"
                        long_triggered = False
                        emit(key, event.seq, "event")
            elif rule.kind == "double_press":
                if event.pressed and not event.repeat:
                    if stage == 0:
                        stage = 1
                        first_press_t = event.t_ms
                        first_press_seq = event.seq
                        pending_deadline_t = event.t_ms + rule.window_ms
                        pending_deadline_kind = "window"
                        pending_source_seq = event.seq
                        state.last_reason = "double_armed"
                        emit(key, event.seq, "event")
                    elif stage == 2:
                        stage = 3
                        state.last_reason = "double_second_down"
                        emit(key, event.seq, "event")
                elif not event.pressed:
                    if stage == 1:
                        stage = 2
                        state.last_reason = "double_first_released"
                        emit(key, event.seq, "event")
                    elif stage == 3:
                        if (
                            first_press_t is not None
                            and event.t_ms - first_press_t <= rule.window_ms
                        ):
                            state.trigger_count += 1
                            state.trigger_t_ms = event.t_ms
                            state.trigger_seq = event.seq
                            state.last_reason = "double_triggered"
                            stage = 0
                            first_press_t = None
                            first_press_seq = None
                            pending_deadline_t = None
                            pending_deadline_kind = None
                            pending_source_seq = None
                            emit(key, event.seq, "event", triggered_now=True)
                        else:
                            state.last_reason = "double_window_expired"
                            stage = 0
                            first_press_t = None
                            first_press_seq = None
                            pending_deadline_t = None
                            pending_deadline_kind = None
                            pending_source_seq = None
                            emit(key, event.seq, "event")
        maybe_process_after(group_t)

    while pending_deadline_t is not None:
        process_timer()

    source_events = (event for event in events if event.seq in set(relevant_seqs))
    return RuleTimeline(
        rule_id=rule.id,
        action=rule.action,
        ticks=tuple(ticks),
        final_state=state.initial(),
        window_ids=_rule_window_ids(rule, source_events, window_size),
    )


def assemble_result(
    bundle: SampleBundle,
    axis_frames: dict[str, tuple[AxisFrame, ...]],
    timelines: dict[str, RuleTimeline],
    *,
    window_size: int = 32,
) -> ReplayResult:
    event_map = {event.seq: event for event in bundle.events}
    axis_frame_by_seq: dict[int, AxisFrame] = {}
    for frames in axis_frames.values():
        for frame in frames:
            axis_frame_by_seq[frame.seq] = frame

    keys: set[TickKey] = {
        TickKey(event.t_ms, 1, event.seq, "") for event in bundle.events
    }
    tick_maps: dict[str, dict[TickKey, RuleTick]] = {}
    for rule_id, timeline in timelines.items():
        tick_maps[rule_id] = {tick.key: tick for tick in timeline.ticks}
        keys.update(tick_maps[rule_id])

    ordered_keys = sorted(keys)
    action_ids = sorted(timeline.action for timeline in timelines.values())
    rule_by_action = {
        timeline.action: rule_id for rule_id, timeline in timelines.items()
    }
    current: dict[str, ActionSnapshot] = {
        action: ActionSnapshot(action=action, last_reason="idle")
        for action in action_ids
    }
    latest_axis_values = {axis.id: 0.0 for axis in bundle.device.axes}
    frames: list[Frame] = []
    for frame_index, key in enumerate(ordered_keys):
        source_event_seq = key.order
        if key.phase == 1:
            event = event_map.get(key.order)
            if event is not None:
                source_event_seq = event.seq
                if event.type == "axis" and event.axis is not None:
                    axis_frame = axis_frame_by_seq.get(event.seq)
                    if axis_frame is not None:
                        latest_axis_values[event.axis] = axis_frame.normalized
            timer_rule = None
            tick_kind = "event"
        elif key.phase == 0:
            timer_rule = key.tag or None
            tick_kind = "long_deadline"
        else:
            timer_rule = key.tag or None
            tick_kind = "window_deadline"
        for rule_id, tick_map in tick_maps.items():
            tick = tick_map.get(key)
            if tick is not None:
                current[tick.action] = tick.state
                source_event_seq = tick.source_event_seq
        actions = {
            action: current[action]
            for action in sorted(current)
        }
        frames.append(
            Frame(
                frame_index=frame_index,
                t_ms=key.t_ms,
                source_event_seq=source_event_seq,
                tick_kind=tick_kind,
                timer_rule=timer_rule,
                tick_key=key,
                axis_values={axis: latest_axis_values[axis] for axis in sorted(latest_axis_values)},
                actions=actions,
            )
        )
        for action in list(current):
            if current[action].triggered_now:
                current[action] = replace(current[action], triggered_now=False)

    return ReplayResult(
        frames=tuple(frames),
        input_hash=bundle.input_hash,
        binding_version=bundle.binding_version,
        algorithm_version=ALGORITHM_VERSION,
        axis_ids=bundle.device.axis_ids,
        action_ids=tuple(action_ids),
    )


def replay_full(
    bundle: SampleBundle,
    *,
    max_event_buffer: int = 256,
    window_size: int = 32,
) -> ReplayResult:
    axis_frames = compute_all_axis_frames(
        bundle,
        max_event_buffer=max_event_buffer,
    )
    axis_frame_map = {
        frame.seq: frame
        for frames in axis_frames.values()
        for frame in frames
    }
    rule_ranks = {
        rule.id: rank
        for rank, rule in enumerate(sorted(bundle.bindings.rules, key=lambda item: item.id))
    }
    timelines = {
        rule.id: replay_rule(
            rule,
            bundle.device.axis_map.get(rule.source_axis or ""),
            bundle.events,
            axis_frame_map,
            rule_rank=rule_ranks[rule.id],
            max_event_buffer=max_event_buffer,
            window_size=window_size,
        )
        for rule in bundle.bindings.rules
    }
    return assemble_result(
        bundle,
        axis_frames,
        timelines,
        window_size=window_size,
    )


@dataclass(frozen=True)
class TruthCheck:
    kind: str
    t_ms: int
    action: str | None
    field: str
    expected: Any
    actual: Any
    passed: bool
    message: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "t_ms": self.t_ms,
            "action": self.action,
            "field": self.field,
            "expected": self.expected,
            "actual": self.actual,
            "passed": self.passed,
            "message": self.message,
        }


@dataclass(frozen=True)
class TruthReport:
    passed: bool
    checks: tuple[TruthCheck, ...]

    @property
    def failures(self) -> tuple[TruthCheck, ...]:
        return tuple(check for check in self.checks if not check.passed)

    def to_dict(self) -> dict[str, Any]:
        return {
            "passed": self.passed,
            "check_count": len(self.checks),
            "failure_count": len(self.failures),
            "checks": [check.to_dict() for check in self.checks],
        }


def check_truth(bundle: SampleBundle, result: ReplayResult) -> TruthReport:
    truth: TruthManifest = bundle.truth
    checks: list[TruthCheck] = []
    tolerance = truth.axis_tolerance
    time_tolerance = truth.time_tolerance_ms

    for sample in truth.axis_samples:
        frame = result.latest_at_or_before(sample.t_ms)
        actual = frame.axis_values.get(sample.axis) if frame else None
        passed = (
            actual is not None
            and sample.minimum - tolerance <= actual <= sample.maximum + tolerance
        )
        checks.append(
            TruthCheck(
                kind="axis_sample",
                t_ms=sample.t_ms,
                action=None,
                field=f"axis.{sample.axis}",
                expected=[sample.minimum, sample.maximum],
                actual=actual,
                passed=passed,
                message="" if passed else "normalized axis value is outside truth range",
            )
        )

    for sample in truth.action_samples:
        frame = result.latest_at_or_before(sample.t_ms)
        snapshot = frame.actions.get(sample.action) if frame else None
        if snapshot is None:
            checks.append(
                TruthCheck(
                    kind="action_sample",
                    t_ms=sample.t_ms,
                    action=sample.action,
                    field="action",
                    expected=sample.to_dict(),
                    actual=None,
                    passed=False,
                    message="action snapshot not found",
                )
            )
            continue
        field_checks = []
        if sample.pressed is not None:
            field_checks.append(("pressed", sample.pressed, snapshot.pressed))
        if sample.trigger_count is not None:
            field_checks.append(
                ("trigger_count", sample.trigger_count, snapshot.trigger_count)
            )
        if sample.repeat_count is not None:
            field_checks.append(("repeat_count", sample.repeat_count, snapshot.repeat_count))
        if sample.reason is not None:
            field_checks.append(("last_reason", sample.reason, snapshot.last_reason))
        for field, expected, actual in field_checks:
            checks.append(
                TruthCheck(
                    kind="action_sample",
                    t_ms=sample.t_ms,
                    action=sample.action,
                    field=field,
                    expected=expected,
                    actual=actual,
                    passed=expected == actual,
                    message="" if expected == actual else "action state differs",
                )
            )

    for sample in truth.trigger_samples:
        candidate_frames = [
            frame
            for frame in result.frames
            if abs(frame.t_ms - sample.t_ms) <= time_tolerance
            and sample.action in frame.actions
        ]
        snapshot = candidate_frames[-1].actions[sample.action] if candidate_frames else None
        passed = (
            snapshot is not None
            and snapshot.trigger_t_ms == sample.t_ms
            and snapshot.trigger_seq == sample.source_event_seq
            and snapshot.trigger_count == sample.trigger_count
        )
        checks.append(
            TruthCheck(
                kind="trigger_sample",
                t_ms=sample.t_ms,
                action=sample.action,
                field="trigger",
                expected=sample.to_dict(),
                actual=snapshot.to_dict() if snapshot else None,
                passed=passed,
                message="" if passed else "trigger time, source event, or count differs",
            )
        )
    return TruthReport(passed=all(check.passed for check in checks), checks=tuple(checks))


__all__ = [
    "Frame",
    "ReplayResult",
    "RuleTick",
    "RuleTimeline",
    "TickKey",
    "TruthCheck",
    "TruthReport",
    "assemble_result",
    "check_truth",
    "compute_all_axis_frames",
    "compute_axis_frames",
    "iter_event_chunks",
    "replay_full",
    "replay_rule",
]
