from __future__ import annotations

import threading
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, Sequence

from .alignment import AlignmentReport
from .codec import encode_stream, encode_stream_parallel
from .dependencies import DependencyGraph, Invalidation
from .errors import ErrorCode, HapticError, Issue, ValidationException
from .limits import ResourceLimits
from .models import (
    DeviceCapabilities,
    Echo,
    EncodedPacket,
    EventStream,
    PacketState,
    ScheduleDecision,
    ScheduledStream,
    canonical_json,
    sha256_hex,
)
from .persistence import CheckpointStore, CheckpointWindow, LoadResult, StoredCheckpoint
from .scheduler import schedule
from .state import PacketStateMachine
from .transport import DispatchRecord, FaultPlan, LoopbackClient, MockDeviceService
from .validation import validate_capabilities, validate_events

PauseGate = Callable[[int], None]


@dataclass(frozen=True)
class RunResult:
    mode: str
    packets: tuple[EncodedPacket, ...]
    decisions: tuple[ScheduleDecision, ...]
    alignment: AlignmentReport
    dependencies: DependencyGraph | None = None
    scheduled: ScheduledStream | None = None
    states: Mapping[int, PacketState] = field(default_factory=dict)
    state_history: tuple[Any, ...] = ()
    invalidations: tuple[Invalidation, ...] = ()
    recovery_issue: Issue | None = None
    reused_count: int = 0
    persisted_windows: int = 0

    def packet(self, packet_seq: int) -> EncodedPacket | None:
        return next((packet for packet in self.packets if packet.packet_seq == packet_seq), None)

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "packets": [packet.to_dict() for packet in self.packets],
            "decisions": [decision.to_dict() for decision in self.decisions],
            "alignment": self.alignment.deterministic_dict(),
            "dependencies": None if self.dependencies is None else self.dependencies.to_dict(),
            "states": {str(seq): state.value for seq, state in sorted(self.states.items())},
            "invalidations": [item.to_dict() for item in self.invalidations],
            "recovery_issue": None if self.recovery_issue is None else self.recovery_issue.to_dict(),
            "reused_count": self.reused_count,
            "persisted_windows": self.persisted_windows,
        }


@dataclass(frozen=True)
class Divergence:
    issue: Issue
    field: str
    left: Any
    right: Any
    packet_seq: int | None
    channel: int | None


@dataclass(frozen=True)
class PathComparison:
    left_mode: str
    right_mode: str
    equal: bool
    divergence: Divergence | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "left": self.left_mode,
            "right": self.right_mode,
            "equal": self.equal,
            "divergence": None
            if self.divergence is None
            else {
                "issue": self.divergence.issue.to_dict(),
                "field": self.divergence.field,
                "left": self.divergence.left,
                "right": self.divergence.right,
                "packet_seq": self.divergence.packet_seq,
                "channel": self.divergence.channel,
            },
        }


@dataclass(frozen=True)
class VerificationSummary:
    realtime: RunResult
    resume: RunResult
    replay: RunResult
    comparisons: tuple[PathComparison, ...]
    service_codes: tuple[str, ...]
    sample_hash: str

    @property
    def ok(self) -> bool:
        return all(item.equal for item in self.comparisons)

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "sample_hash": self.sample_hash,
            "comparisons": [item.to_dict() for item in self.comparisons],
            "service_codes": list(self.service_codes),
            "modes": {
                "realtime": self.realtime.to_dict(),
                "resume": self.resume.to_dict(),
                "replay": self.replay.to_dict(),
            },
        }


def _validate(stream: EventStream, capabilities: DeviceCapabilities) -> None:
    cap_result = validate_capabilities(capabilities)
    event_result = validate_events(stream, capabilities)
    issues = list(cap_result.issues) + list(event_result.issues)
    if issues:
        raise ValidationException(issues)


def _sent_at(packet_seq: int) -> float:
    return round(float(packet_seq) * 2.0, 3)


def _checkpoint_decisions(
    decisions: Sequence[ScheduleDecision], packet_seq: int
) -> tuple[ScheduleDecision, ...]:
    return tuple(item for item in decisions if item.target_packet_seq == packet_seq)


def _event_positions(stream: EventStream) -> dict[str, int]:
    return {event.event_id: index for index, event in enumerate(stream.events)}


def _commit_prefix(
    store: CheckpointStore,
    stream: EventStream,
    capabilities: DeviceCapabilities,
    scheduled: ScheduledStream,
    encoded: Sequence[EncodedPacket],
    kernel_version: str,
    start_index: int = 0,
    pause_gate: PauseGate | None = None,
) -> int:
    positions = _event_positions(stream)
    prefix_offset = 0
    for packet, wire in zip(scheduled.packets, encoded):
        if packet.packet_seq < start_index:
            continue
        if pause_gate is not None:
            pause_gate(packet.packet_seq)
        source_positions = [positions[item] for item in packet.source_event_ids if item in positions]
        if source_positions:
            prefix_offset = max(prefix_offset, max(source_positions) + 1)
        checkpoint = CheckpointWindow(
            order_index=wire.packet_seq,
            packet=wire,
            decisions=_checkpoint_decisions(scheduled.decisions, wire.packet_seq),
            consumed_event_offset=prefix_offset,
            capability_version=capabilities.capability_version,
            capability_fingerprint=capabilities.manifest_fingerprint,
            event_stream_hash=stream.content_hash(),
            kernel_version=kernel_version,
        )
        store.commit_window(checkpoint)
    return store.count()


def _dispatch(
    packets: Sequence[EncodedPacket],
    *,
    service: MockDeviceService,
    run_id: str,
    timeout_ms: float,
    pause_gate: PauseGate | None = None,
) -> tuple[tuple[EncodedPacket, ...], AlignmentReport, PacketStateMachine]:
    state_machine = PacketStateMachine()
    records: list[DispatchRecord] = []
    dispatched: list[EncodedPacket] = []
    for index, packet in enumerate(packets):
        if pause_gate is not None:
            pause_gate(packet.packet_seq)
        state_machine.register(packet.packet_seq, packet.channel, PacketState.PENDING)
        state_machine.transition(packet.packet_seq, PacketState.ENCODED, "encoded")
        state_machine.transition(packet.packet_seq, PacketState.DISPATCHED, "serial dispatch")
        sent = packet.with_state(PacketState.DISPATCHED, _sent_at(packet.packet_seq))
        dispatched.append(sent)
        records.append(
            DispatchRecord(
                packet_seq=packet.packet_seq,
                channel=packet.channel,
                data=packet.data,
                sent_at_ms=float(sent.sent_at_ms or 0.0),
            )
        )
    client = LoopbackClient(service.host, service.port)
    echoes = client.send_batch(run_id, records, timeout_ms)
    report = AlignmentReport.build(dispatched, echoes, timeout_ms)
    final_packets: list[EncodedPacket] = []
    for packet in dispatched:
        observation = report.packet(packet.packet_seq)
        if observation is None or observation.issue is None:
            state_machine.transition(
                packet.packet_seq, PacketState.CONFIRMED, "service echo aligned"
            )
            final_packets.append(packet.with_state(PacketState.CONFIRMED))
        else:
            state_machine.transition(
                packet.packet_seq,
                PacketState.FAILED,
                f"service echo issue {observation.issue.code.value}",
            )
            final_packets.append(packet.with_state(PacketState.FAILED))
    return tuple(final_packets), report, state_machine


def run_realtime(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    *,
    store: CheckpointStore | None = None,
    service: MockDeviceService | None = None,
    fault_plan: FaultPlan | None = None,
    timeout_ms: float = 30.0,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
    kernel_version: str = "1.0.0",
    run_id: str = "realtime",
    pause_gate: PauseGate | None = None,
    reset_store: bool = True,
) -> RunResult:
    _validate(stream, capabilities)
    scheduled = schedule(stream, capabilities, parallel=parallel, limits=limits)
    if parallel:
        encoded = encode_stream_parallel(scheduled, capabilities, limits)
    else:
        encoded = encode_stream(scheduled, capabilities)
    graph = DependencyGraph.build(scheduled, capabilities, encoded)

    if store is not None and reset_store:
        store.clear()
    if store is not None:
        _commit_prefix(
            store,
            stream,
            capabilities,
            scheduled,
            encoded,
            kernel_version,
            pause_gate=pause_gate,
        )

    own_service = service is None
    if service is None:
        service = MockDeviceService(fault_plan=fault_plan or FaultPlan.sample()).start()
    try:
        final_packets, report, states = _dispatch(
            encoded,
            service=service,
            run_id=run_id,
            timeout_ms=timeout_ms,
            pause_gate=pause_gate,
        )
    finally:
        if own_service and service is not None:
            service.stop()
    return RunResult(
        mode="REALTIME",
        packets=final_packets,
        decisions=scheduled.decisions,
        alignment=report,
        dependencies=graph,
        scheduled=scheduled,
        states=states.snapshot(),
        state_history=states.history(),
        persisted_windows=0 if store is None else store.count(),
    )


def _replace_prefix_with_persisted(
    scheduled: ScheduledStream,
    encoded: tuple[EncodedPacket, ...],
    loaded: LoadResult,
) -> tuple[tuple[EncodedPacket, ...], tuple[ScheduleDecision, ...], int]:
    if not loaded.checkpoints:
        return encoded, scheduled.decisions, 0
    stored_by_seq = {item.packet.packet_seq: item for item in loaded.checkpoints}
    merged: list[EncodedPacket] = []
    reused = 0
    for packet in encoded:
        stored = stored_by_seq.get(packet.packet_seq)
        if stored is None:
            merged.append(packet)
            continue
        merged.append(
            replace(
                stored.packet,
                state=PacketState.ENCODED,
                sent_at_ms=None,
                reused=True,
            )
        )
        reused += 1
    stored_decisions = {
        decision.decision_id: decision
        for checkpoint in loaded.checkpoints
        for decision in checkpoint.decisions
    }
    decisions = tuple(stored_decisions.get(item.decision_id, item) for item in scheduled.decisions)
    return tuple(merged), decisions, reused


def run_resume(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    store: CheckpointStore,
    *,
    service: MockDeviceService | None = None,
    fault_plan: FaultPlan | None = None,
    timeout_ms: float = 30.0,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
    kernel_version: str = "1.0.0",
    run_id: str = "resume",
    pause_gate: PauseGate | None = None,
) -> RunResult:
    _validate(stream, capabilities)
    loaded = store.load_valid(
        expected_capability_version=capabilities.capability_version,
        expected_kernel_version=kernel_version,
        invalidate_on_error=True,
    )
    recovery_issue = loaded.issue
    if loaded.valid and loaded.checkpoints:
        active_stream_hash = stream.content_hash()
        active_capability_fingerprint = capabilities.manifest_fingerprint
        snapshot_mismatch = any(
            checkpoint.event_stream_hash != active_stream_hash
            or checkpoint.capability_fingerprint != active_capability_fingerprint
            for checkpoint in loaded.checkpoints
        )
        if snapshot_mismatch:
            recovery_issue = Issue(
                ErrorCode.E_HASH,
                "persisted event stream or capability snapshot changed; full replay is required",
                field="event_stream_hash",
                details={
                    "active_stream_hash": active_stream_hash,
                    "active_capability_fingerprint": active_capability_fingerprint,
                },
            )
            loaded = LoadResult(valid=False, checkpoints=(), issue=recovery_issue, invalidated=True)
    if not loaded.valid:
        full = run_realtime(
            stream,
            capabilities,
            store=store,
            service=service,
            fault_plan=fault_plan,
            timeout_ms=timeout_ms,
            parallel=parallel,
            limits=limits,
            kernel_version=kernel_version,
            run_id=run_id,
            pause_gate=pause_gate,
            reset_store=True,
        )
        return replace(
            full,
            mode="RESUME_FULL_REPLAY",
            recovery_issue=recovery_issue,
            persisted_windows=store.count(),
        )

    scheduled = schedule(stream, capabilities, parallel=parallel, limits=limits)
    encoded = (
        encode_stream_parallel(scheduled, capabilities, limits)
        if parallel
        else encode_stream(scheduled, capabilities)
    )
    merged, decisions, reused = _replace_prefix_with_persisted(scheduled, encoded, loaded)
    graph = DependencyGraph.build(scheduled, capabilities, merged)
    missing_start = store.count()
    if missing_start < len(scheduled.packets):
        _commit_prefix(
            store,
            stream,
            capabilities,
            scheduled,
            merged,
            kernel_version,
            start_index=missing_start,
            pause_gate=pause_gate,
        )

    own_service = service is None
    if service is None:
        service = MockDeviceService(fault_plan=fault_plan or FaultPlan.sample()).start()
    try:
        final_packets, report, states = _dispatch(
            merged,
            service=service,
            run_id=run_id,
            timeout_ms=timeout_ms,
            pause_gate=pause_gate,
        )
    finally:
        if own_service and service is not None:
            service.stop()
    return RunResult(
        mode="RESUME",
        packets=final_packets,
        decisions=decisions,
        alignment=report,
        dependencies=graph,
        scheduled=scheduled,
        states=states.snapshot(),
        state_history=states.history(),
        recovery_issue=recovery_issue,
        reused_count=reused,
        persisted_windows=store.count(),
    )


def run_replay_from_persisted(
    capabilities: DeviceCapabilities,
    store: CheckpointStore,
    *,
    service: MockDeviceService | None = None,
    fault_plan: FaultPlan | None = None,
    timeout_ms: float = 30.0,
    kernel_version: str = "1.0.0",
    run_id: str = "replay",
    pause_gate: PauseGate | None = None,
) -> RunResult:
    loaded = store.load_valid(
        expected_capability_version=capabilities.capability_version,
        expected_kernel_version=kernel_version,
        invalidate_on_error=True,
    )
    if not loaded.valid or not loaded.checkpoints:
        issue = loaded.issue or Issue(
            ErrorCode.E_HASH,
            "no valid persisted checkpoints are available for replay",
            field="checkpoints",
        )
        raise HapticError(issue)
    packets = tuple(
        replace(checkpoint.packet, state=PacketState.ENCODED, sent_at_ms=None)
        for checkpoint in loaded.checkpoints
    )
    decisions = tuple(
        decision
        for checkpoint in loaded.checkpoints
        for decision in checkpoint.decisions
    )
    decisions = tuple(sorted(decisions, key=lambda item: item.decision_id))
    own_service = service is None
    if service is None:
        service = MockDeviceService(fault_plan=fault_plan or FaultPlan.sample()).start()
    try:
        final_packets, report, states = _dispatch(
            packets,
            service=service,
            run_id=run_id,
            timeout_ms=timeout_ms,
            pause_gate=pause_gate,
        )
    finally:
        if own_service and service is not None:
            service.stop()
    return RunResult(
        mode="PERSISTED_REPLAY",
        packets=final_packets,
        decisions=decisions,
        alignment=report,
        dependencies=None,
        scheduled=None,
        states=states.snapshot(),
        state_history=states.history(),
        persisted_windows=store.count(),
    )


def _decision_value(decision: ScheduleDecision) -> dict[str, Any]:
    return decision.to_dict()


def compare_results(left: RunResult, right: RunResult) -> PathComparison:
    left_by_seq = {packet.packet_seq: packet for packet in left.packets}
    right_by_seq = {packet.packet_seq: packet for packet in right.packets}
    all_seq = sorted(set(left_by_seq) | set(right_by_seq))
    for seq in all_seq:
        lpacket = left_by_seq.get(seq)
        rpacket = right_by_seq.get(seq)
        channel = (lpacket or rpacket).channel if (lpacket or rpacket) else None
        if lpacket is None or rpacket is None:
            divergence = Divergence(
                issue=Issue(
                    ErrorCode.E_DIVERGE,
                    "packet exists on only one execution path",
                    packet_seq=seq,
                    channel=channel,
                    field="packet_seq",
                    details={"left_present": lpacket is not None, "right_present": rpacket is not None},
                ),
                field="packet_seq",
                left=lpacket is not None,
                right=rpacket is not None,
                packet_seq=seq,
                channel=channel,
            )
            return PathComparison(left.mode, right.mode, False, divergence)
        for field_name, left_value, right_value in (
            ("channel", lpacket.channel, rpacket.channel),
            ("data_hex", lpacket.data.hex(), rpacket.data.hex()),
            ("timing_mark_ms", lpacket.timing_mark_ms, rpacket.timing_mark_ms),
            ("event_count", lpacket.event_count, rpacket.event_count),
        ):
            if left_value != right_value:
                divergence = Divergence(
                    issue=Issue(
                        ErrorCode.E_DIVERGE,
                        "execution paths diverged",
                        packet_seq=seq,
                        channel=lpacket.channel,
                        field=field_name,
                        details={"left": left_value, "right": right_value},
                    ),
                    field=field_name,
                    left=left_value,
                    right=right_value,
                    packet_seq=seq,
                    channel=lpacket.channel,
                )
                return PathComparison(left.mode, right.mode, False, divergence)

    left_decisions = [_decision_value(item) for item in left.decisions]
    right_decisions = [_decision_value(item) for item in right.decisions]
    if left_decisions != right_decisions:
        for index in range(min(len(left_decisions), len(right_decisions))):
            if left_decisions[index] != right_decisions[index]:
                ldec = left_decisions[index]
                rdec = right_decisions[index]
                for field_name in sorted(set(ldec) | set(rdec)):
                    if ldec.get(field_name) != rdec.get(field_name):
                        target = ldec.get("target_packet_seq")
                        divergence = Divergence(
                            issue=Issue(
                                ErrorCode.E_DIVERGE,
                                "decision sequence diverged",
                                packet_seq=None if target is None or target < 0 else int(target),
                                channel=int(ldec.get("channel", rdec.get("channel", 0))),
                                field=f"decision.{field_name}",
                                details={
                                    "decision_index": index,
                                    "left": ldec.get(field_name),
                                    "right": rdec.get(field_name),
                                },
                            ),
                            field=f"decision.{field_name}",
                            left=ldec.get(field_name),
                            right=rdec.get(field_name),
                            packet_seq=None if target is None or target < 0 else int(target),
                            channel=int(ldec.get("channel", rdec.get("channel", 0))),
                        )
                        return PathComparison(left.mode, right.mode, False, divergence)
        divergence = Divergence(
            issue=Issue(
                ErrorCode.E_DIVERGE,
                "decision sequence length diverged",
                field="decisions.length",
            ),
            field="decisions.length",
            left=len(left_decisions),
            right=len(right_decisions),
            packet_seq=None,
            channel=None,
        )
        return PathComparison(left.mode, right.mode, False, divergence)

    left_alignment = left.alignment.deterministic_dict()
    right_alignment = right.alignment.deterministic_dict()
    if left_alignment != right_alignment:
        left_packets = {item["packet_seq"]: item for item in left_alignment["packets"]}
        right_packets = {item["packet_seq"]: item for item in right_alignment["packets"]}
        for seq in sorted(set(left_packets) | set(right_packets)):
            lval = left_packets.get(seq)
            rval = right_packets.get(seq)
            if lval == rval:
                continue
            lstatus = None if lval is None else lval.get("status")
            rstatus = None if rval is None else rval.get("status")
            channel = None
            if lval is not None:
                channel = int(lval["channel"])
            elif rval is not None:
                channel = int(rval["channel"])
            divergence = Divergence(
                issue=Issue(
                    ErrorCode.E_DIVERGE,
                    "alignment result diverged",
                    packet_seq=seq,
                    channel=channel,
                    field="alignment.status",
                    details={"left": lstatus, "right": rstatus},
                ),
                field="alignment.status",
                left=lstatus,
                right=rstatus,
                packet_seq=seq,
                channel=channel,
            )
            return PathComparison(left.mode, right.mode, False, divergence)
    return PathComparison(left.mode, right.mode, True, None)


def retry_alignment_packet(
    result: RunResult,
    packet_seq: int,
    service: MockDeviceService,
    *,
    timeout_ms: float = 30.0,
    run_id: str = "retry",
) -> RunResult:
    """Retry exactly one packet and update only that packet's observation."""

    packet = result.packet(packet_seq)
    if packet is None:
        raise KeyError(f"packet {packet_seq} is not present")
    observation = result.alignment.packet(packet_seq)
    if observation is not None and observation.issue is not None:
        if observation.issue.code == ErrorCode.E_LOCAL:
            raise HapticError(
                Issue(
                    ErrorCode.E_LOCAL,
                    "local kernel packet-order violations are not retryable at the service layer",
                    packet_seq=packet_seq,
                    channel=packet.channel,
                    field="packet_seq",
                    first_seen_ms=observation.issue.first_seen_ms,
                )
            )
    client = LoopbackClient(service.host, service.port)
    echo = client.retry_packet(
        f"{run_id}:{packet_seq}",
        DispatchRecord(
            packet_seq=packet.packet_seq,
            channel=packet.channel,
            data=packet.data,
            sent_at_ms=float(packet.sent_at_ms or _sent_at(packet.packet_seq)),
        ),
        timeout_ms,
    )
    report = result.alignment.with_retry(packet_seq, echo)
    updated_packets = tuple(
        replace(
            item,
            state=PacketState.CONFIRMED if item.packet_seq == packet_seq else item.state,
        )
        for item in result.packets
    )
    states = dict(result.states)
    if packet_seq in states:
        states[packet_seq] = PacketState.CONFIRMED
    return replace(
        result,
        packets=updated_packets,
        alignment=report,
        states=states,
    )


def verify_three_paths(
    stream: EventStream,
    capabilities: DeviceCapabilities,
    workdir: str | Path,
    *,
    fault_plan: FaultPlan | None = None,
    timeout_ms: float = 30.0,
    parallel: bool = False,
    limits: ResourceLimits = ResourceLimits(),
    kernel_version: str = "1.0.0",
) -> VerificationSummary:
    plan = fault_plan or FaultPlan.sample()
    store = CheckpointStore(workdir)
    try:
        with MockDeviceService(fault_plan=plan) as realtime_service:
            realtime = run_realtime(
                stream,
                capabilities,
                store=store,
                service=realtime_service,
                timeout_ms=timeout_ms,
                parallel=parallel,
                limits=limits,
                kernel_version=kernel_version,
                run_id="verify-realtime",
            )
        with MockDeviceService(fault_plan=plan) as resume_service:
            resume = run_resume(
                stream,
                capabilities,
                store,
                service=resume_service,
                timeout_ms=timeout_ms,
                parallel=parallel,
                limits=limits,
                kernel_version=kernel_version,
                run_id="verify-resume",
            )
        with MockDeviceService(fault_plan=plan) as replay_service:
            replay = run_replay_from_persisted(
                capabilities,
                store,
                service=replay_service,
                timeout_ms=timeout_ms,
                kernel_version=kernel_version,
                run_id="verify-replay",
            )
        comparisons = (
            compare_results(realtime, resume),
            compare_results(realtime, replay),
            compare_results(resume, replay),
        )
        codes = tuple(
            sorted(
                {
                    item.code.value
                    for item in realtime.alignment.issues
                }
            )
        )
        return VerificationSummary(
            realtime=realtime,
            resume=resume,
            replay=replay,
            comparisons=comparisons,
            service_codes=codes,
            sample_hash=stream.content_hash(),
        )
    finally:
        store.close()

