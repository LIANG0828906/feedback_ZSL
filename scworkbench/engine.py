from __future__ import annotations

import base64
import binascii
import copy
import hashlib
import threading
import zlib
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from .anomalies import AnomalyConfig, FragmentFault
from .checkpoint import CheckpointManager
from .concurrency import BoundedExecutor
from .desktop import DesktopModel
from .errors import (
    CapabilityChangeError,
    CorruptError,
    SchemaError,
    SessionMismatchError,
    StateError,
    WorkbenchError,
)
from .models import (
    FrameResult,
    FrameSpec,
    InputEvent,
    InputResult,
    LEGAL_TRANSITIONS,
    NegotiationResult,
    Phase,
    PhaseEvent,
    RunResult,
    SampleSession,
    SessionStats,
    TimelineItem,
)
from .negotiation import negotiate
from .profiles import CODEC_PROFILES, PROTOCOL_VERSION, CodecProfile
from .protocol import EncodedFrame, InputEnvelope, MediaPacket, encode_frame_payload
from .reassembly import FrameReassembler, ReassembledFrame
from .rendering import image_digest, render_missing_frame, render_scene
from .transport import DirectTransport, LoopbackTransport


@dataclass
class EngineConfig:
    retry_limit: int = 2
    checkpoint_every: int = 5
    max_parallelism: int = 2
    per_thread_buffer: int = 4
    anomaly: AnomalyConfig = field(default_factory=AnomalyConfig)
    enable_checkpoints: bool = True
    initial_tier: str | None = None


@dataclass
class TransmissionOutcome:
    reassembled: ReassembledFrame
    packets_sent: int
    packets_received: int
    retransmissions: int
    codes: list[str]
    fragment_order: list[int]
    fragment_status: list[str]


class SessionEngine:
    def __init__(
        self,
        sample: SampleSession,
        config: EngineConfig | None = None,
        checkpoint_manager: CheckpointManager | None = None,
        transport: LoopbackTransport | None = None,
        negotiation: NegotiationResult | None = None,
    ) -> None:
        self.sample = sample
        self.config = config or EngineConfig()
        if self.config.max_parallelism < 1 or self.config.max_parallelism > 8:
            raise SchemaError("max_parallelism 必须在 1..8")
        if self.config.per_thread_buffer < 1 or self.config.per_thread_buffer > 64:
            raise SchemaError("per_thread_buffer 必须在 1..64")
        if not 0 <= self.config.retry_limit <= 16:
            raise SchemaError("retry_limit 必须在 0..16")
        self.negotiation = negotiation or negotiate(sample)
        self.checkpoint_manager = checkpoint_manager
        self.transport = transport or DirectTransport()
        self.phase = Phase.NEGOTIATING
        self._lock = threading.RLock()
        self._started = False
        self._closed = False
        self._executor = BoundedExecutor(
            self.config.max_parallelism,
            self.config.max_parallelism * self.config.per_thread_buffer,
            label="cast-codec",
        )
        self._timeline = self._build_timeline()
        self._timeline_index = 0
        self._frames_by_seq = {frame.seq: frame for frame in sample.frames}
        self._inputs_by_seq = {event.seq: event for event in sample.inputs}
        self._frame_results: list[FrameResult] = []
        self._input_results: list[InputResult] = []
        self._phase_events: list[PhaseEvent] = []
        self._events: list[dict[str, Any]] = []
        self._stats = SessionStats(inputs_total=len(sample.inputs))
        self._desktop = DesktopModel(
            declared_width=sample.declared_width,
            declared_height=sample.declared_height,
            width=self.negotiation.width,
            height=self.negotiation.height,
        )
        self._seen_input_seqs: set[int] = set()
        self._last_replayed_seq = 0
        self._last_image: np.ndarray | None = None
        self._last_image_sha = ""
        self._allowed_tiers = tuple(
            sorted(
                set(sample.sender.capabilities.codecs).intersection(
                    sample.receiver.capabilities.codecs
                ),
                key=lambda name: CODEC_PROFILES[name].rank,
                reverse=True,
            )
        )
        self._runtime_tier = self.negotiation.codec
        if self.config.initial_tier:
            if self.config.initial_tier not in self._allowed_tiers:
                raise CapabilityChangeError(
                    "初始编码档位不在协商交集内",
                    items=[f"codec:{self.config.initial_tier}"],
                    allowed=list(self._allowed_tiers),
                )
            self._runtime_tier = self.config.initial_tier
        self._retry_limit = self.config.retry_limit
        self._checkpoint_count = 0
        self._force_checkpoint = False
        self._capability_event_emitted = False
        self._retrying_frame = False

    def _build_timeline(self) -> list[TimelineItem]:
        entries: list[tuple[int, int, str, int]] = []
        for index, frame in enumerate(self.sample.frames):
            entries.append((frame.timestamp_ms, 1, "frame", index))
        for index, event in enumerate(self.sample.inputs):
            entries.append((event.timestamp_ms, 2, "input", index))
        for index, action in enumerate(self.sample.phase_script):
            entries.append((action.timestamp_ms, 0, "phase", index))
        entries.sort(key=lambda item: (item[0], item[1], item[3]))
        return [
            TimelineItem(index=index, timestamp_ms=item[0], kind=item[2], payload_index=item[3])
            for index, item in enumerate(entries)
        ]

    @property
    def current_image(self) -> np.ndarray | None:
        with self._lock:
            return None if self._last_image is None else self._last_image.copy()

    @property
    def stats(self) -> SessionStats:
        with self._lock:
            return copy.deepcopy(self._stats)

    @property
    def runtime_tier(self) -> str:
        with self._lock:
            return self._runtime_tier

    @property
    def retry_limit(self) -> int:
        with self._lock:
            return self._retry_limit

    @property
    def timeline_index(self) -> int:
        return self._timeline_index

    @property
    def finished(self) -> bool:
        return self._timeline_index >= len(self._timeline)

    def _emit_event(self, kind: str, **data: Any) -> None:
        entry = {
            "index": len(self._events) + 1,
            "type": kind,
            "timestamp_ms": int(data.pop("timestamp_ms", 0)),
            "phase": self.phase.value,
            "frames_committed": self._stats.frames_committed,
            "inputs_replayed": self._stats.inputs_replayed,
            **data,
        }
        self._events.append(entry)

    def _transition(
        self, target: Phase, timestamp_ms: int, action: str
    ) -> PhaseEvent:
        old_phase = self.phase
        ok = target in LEGAL_TRANSITIONS[old_phase]
        if ok:
            self.phase = target
        event = PhaseEvent(
            index=len(self._phase_events) + 1,
            timestamp_ms=timestamp_ms,
            action=action,
            from_phase=old_phase.value,
            to_phase=self.phase.value,
            ok=ok,
            code=None if ok else "E_STATE",
            message=(
                f"{old_phase.label} -> {target.label}"
                if ok
                else f"非法迁移 {old_phase.label} -> {target.label}"
            ),
            frames_committed=self._stats.frames_committed,
            inputs_replayed=self._stats.inputs_replayed,
        )
        self._phase_events.append(event)
        self._emit_event(
            "phase_transition",
            timestamp_ms=timestamp_ms,
            action=action,
            from_phase=old_phase.value,
            to_phase=self.phase.value,
            ok=ok,
            code=event.code,
        )
        if not ok:
            self._stats.phase_rejections += 1
            self._force_checkpoint = True
        return event

    def start(self) -> None:
        with self._lock:
            if self._started:
                return
            if self.phase != Phase.NEGOTIATING:
                raise StateError(
                    "会话只能从协商中阶段启动", self.phase.value
                )
            self._transition(Phase.MIRRORING, 0, "establish")
            self._started = True

    def _handle_phase_action(self, action: str, timestamp_ms: int) -> None:
        target = {
            "pause": Phase.PAUSED,
            "resume": Phase.MIRRORING,
            "reconnect": Phase.RECONNECTING,
            "reconnect_complete": Phase.MIRRORING,
            "disconnect": Phase.DISCONNECTED,
        }[action]
        event = self._transition(target, timestamp_ms, action)
        self._force_checkpoint = True
        if not event.ok:
            return

    def _capability_change_codes(self, frame_seq: int) -> list[str]:
        anomaly = self.config.anomaly
        if (
            anomaly.capability_change_frame != frame_seq
            or self._capability_event_emitted
        ):
            return []
        self._capability_event_emitted = True
        self._stats.capability_changes_rejected += 1
        changed_items: list[str] = []
        if (
            anomaly.capability_change_codecs is not None
            and self.negotiation.codec not in anomaly.capability_change_codecs
        ):
            changed_items.append(f"codec:{self.negotiation.codec}")
        if (
            anomaly.capability_change_max_width is not None
            and self.negotiation.width > anomaly.capability_change_max_width
        ):
            changed_items.append("resolution:width")
        if (
            anomaly.capability_change_max_height is not None
            and self.negotiation.height > anomaly.capability_change_max_height
        ):
            changed_items.append("resolution:height")
        if not changed_items:
            changed_items.append("capability_set")
        self._emit_event(
            "capability_change_rejected",
            timestamp_ms=self._frames_by_seq[frame_seq].timestamp_ms,
            frame_seq=frame_seq,
            code="E_CAP",
            items=changed_items,
        )
        return ["E_CAP"]

    def _frame_rejected(self, frame: FrameSpec) -> FrameResult:
        self._stats.frames_rejected += 1
        self._stats.phase_rejections += 1
        result = FrameResult(
            frame_seq=frame.seq,
            timestamp_ms=frame.timestamp_ms,
            phase=self.phase.value,
            status="rejected",
            committed=False,
            codec=self._runtime_tier,
            width=self.negotiation.width,
            height=self.negotiation.height,
            frame_sha256=self._last_image_sha,
            wire_sha256="",
            packets_sent=0,
            packets_received=0,
            fragments_total=0,
            fragments_received=0,
            retransmissions=0,
            missing_fragments=[],
            codes=["E_STATE"],
            message="帧在非镜像中阶段到达，已拒绝且状态未改变",
            fragment_order=[],
            fragment_status=[],
        )
        self._frame_results.append(result)
        self._emit_event(
            "frame_rejected",
            timestamp_ms=frame.timestamp_ms,
            frame_seq=frame.seq,
            code="E_STATE",
        )
        return result

    def _wire_media(self, raw: bytes) -> bytes:
        return self.transport.send_media(raw)

    def _wire_input(self, raw: bytes) -> bytes:
        return self.transport.send_input(raw)

    @staticmethod
    def _fault_applies(
        fault: FragmentFault, fragment_index: int, attempt: int
    ) -> bool:
        if fragment_index not in fault.fragments:
            return False
        return fault.attempts is None or attempt < fault.attempts

    def _injected_media(
        self,
        packet: MediaPacket,
        frame_seq: int,
        attempt: int,
    ) -> tuple[bytes | None, str | None]:
        anomaly = self.config.anomaly
        raw: bytes = packet.to_bytes()
        for fault in anomaly.loss_faults_for(frame_seq):
            if self._fault_applies(fault, packet.fragment_index, attempt):
                return None, "E_LOSS"
        for fault in anomaly.corrupt_faults_for(frame_seq):
            if self._fault_applies(fault, packet.fragment_index, attempt):
                wire = bytearray(packet.to_bytes())
                wire[-1] ^= 0xA5
                return bytes(wire), "E_CORRUPT"
        return raw, None

    def _transmit_frame(
        self, encoded: EncodedFrame, frame: FrameSpec
    ) -> TransmissionOutcome:
        assembler = FrameReassembler(
            self.sample.session_id,
            self.negotiation.width,
            self.negotiation.height,
            max_fragment_count=max(1, min(65535, 4096)),
        )
        codes: set[str] = set()
        fragment_order: list[int] = []
        fragment_status = ["pending"] * len(encoded.packets)
        retransmitted: set[int] = set()
        packets_sent = 0
        packets_received = 0
        retransmission_requests = 0
        reorder = self.config.anomaly.should_reorder(frame.seq)

        initial_items: list[tuple[bytes | None, str | None, int]] = []
        if frame.seq in self.config.anomaly.session_mismatch_frames:
            foreign = encoded.packets[0].with_foreign_session().to_bytes()
            echo = self._wire_media(foreign)
            packets_sent += 1
            initial_items.append(
                (echo if echo != foreign else foreign, "E_SESSION", -1)
            )
        for packet in encoded.packets:
            echo = self._wire_media(packet.to_bytes())
            packets_sent += 1
            injected, hint = self._injected_media(packet, frame.seq, 0)
            if echo != packet.to_bytes():
                injected = echo
            initial_items.append((injected, hint, packet.fragment_index))
        if reorder:
            initial_items.reverse()

        def feed(item: tuple[bytes | None, str | None, int], is_retry: bool) -> None:
            nonlocal packets_received
            raw, hint, index = item
            if raw is None:
                if index >= 0:
                    fragment_status[index] = "lost"
                if hint:
                    codes.add(hint)
                return
            packets_received += 1
            try:
                accepted = assembler.accept_bytes(raw)
                if accepted is not None and index >= 0:
                    fragment_order.append(accepted.fragment_index)
                if index >= 0:
                    fragment_status[index] = (
                        "retransmitted"
                        if is_retry or index in retransmitted
                        else "ok"
                    )
            except SessionMismatchError:
                if index >= 0:
                    fragment_status[index] = "session-rejected"
                codes.add("E_SESSION")
                self._stats.packets_session_rejected += 1
            except CorruptError:
                if index >= 0:
                    fragment_status[index] = "corrupt"
                codes.add("E_CORRUPT")
                self._stats.packets_corrupt += 1
            except WorkbenchError:
                if index >= 0:
                    fragment_status[index] = "corrupt"
                codes.add("E_CORRUPT")
                self._stats.packets_corrupt += 1

        for item in initial_items:
            feed(item, False)

        missing = assembler.missing(len(encoded.packets))
        if missing:
            codes.add("E_LOSS")
        attempt = 0
        while missing and attempt < self._retry_limit:
            attempt += 1
            retransmission_requests += len(missing)
            self._stats.retransmission_requests += len(missing)
            before = len(missing)
            for index in missing:
                retransmitted.add(index)
                packet = encoded.packets[index]
                echo = self._wire_media(packet.to_bytes())
                packets_sent += 1
                injected, hint = self._injected_media(packet, frame.seq, attempt)
                if echo != packet.to_bytes():
                    injected = echo
                feed((injected, hint, index), True)
            missing = assembler.missing(len(encoded.packets))
            self._stats.retransmission_recovered += max(0, before - len(missing))

        reassembled = assembler.finalize()
        codes.update(reassembled.codes)
        for index in reassembled.missing_fragments:
            fragment_status[index] = "missing"
        self._stats.packets_sent += packets_sent
        self._stats.packets_received += packets_received
        self._stats.fragments_sent += packets_sent
        self._stats.fragments_received += assembler.received_count
        self._stats.order_events += assembler.order_events
        if reassembled.status != "complete":
            self._stats.fragments_lost += len(reassembled.missing_fragments)
        return TransmissionOutcome(
            reassembled=reassembled,
            packets_sent=packets_sent,
            packets_received=packets_received,
            retransmissions=retransmission_requests,
            codes=sorted(codes),
            fragment_order=fragment_order,
            fragment_status=fragment_status,
        )

    def _handle_frame(self, frame: FrameSpec) -> None:
        if self.phase != Phase.MIRRORING:
            self._frame_rejected(frame)
            return
        capability_codes = self._capability_change_codes(frame.seq)
        profile: CodecProfile = CODEC_PROFILES[self._runtime_tier]
        self._desktop.observe_scene(frame.scene, frame.seq)
        modifiers = self._desktop.render_modifiers(frame.seq)
        try:
            future = self._executor.submit(
                render_scene,
                frame.scene,
                self.negotiation.width,
                self.negotiation.height,
                self.sample.declared_width,
                self.sample.declared_height,
                modifiers,
            )
            source_image = future.result()
            encoded = encode_frame_payload(
                source_image,
                frame.seq,
                frame.timestamp_ms,
                self.sample.session_id,
                profile,
            )
            outcome = self._transmit_frame(encoded, frame)
            if outcome.reassembled.image is not None:
                image = outcome.reassembled.image
                status = "committed"
            else:
                image = render_missing_frame(
                    self.negotiation.width,
                    self.negotiation.height,
                    frame.seq,
                    frame.timestamp_ms,
                )
                status = "missing"
            self._stats.frames_sent += 1
            self._stats.frames_reassembled += 1
            self._stats.frames_committed += 1
            if status == "missing":
                self._stats.frames_missing += 1
            self._last_image = image
            self._last_image_sha = image_digest(image)
            codes = capability_codes + outcome.codes
            result = FrameResult(
                frame_seq=frame.seq,
                timestamp_ms=frame.timestamp_ms,
                phase=self.phase.value,
                status=status,
                committed=True,
                codec=profile.name,
                width=self.negotiation.width,
                height=self.negotiation.height,
                frame_sha256=self._last_image_sha,
                wire_sha256=encoded.wire_sha256,
                packets_sent=outcome.packets_sent,
                packets_received=outcome.packets_received,
                fragments_total=len(encoded.packets),
                fragments_received=outcome.reassembled.received_fragments,
                retransmissions=outcome.retransmissions,
                missing_fragments=list(outcome.reassembled.missing_fragments),
                codes=codes,
                message=outcome.reassembled.message,
                fragment_order=outcome.fragment_order,
                fragment_status=outcome.fragment_status,
            )
            self._frame_results.append(result)
            self._emit_event(
                "frame_committed",
                timestamp_ms=frame.timestamp_ms,
                frame_seq=frame.seq,
                status=status,
                codes=codes,
                packets_sent=outcome.packets_sent,
                packets_received=outcome.packets_received,
                frame_sha256=self._last_image_sha,
            )
        except Exception as exc:
            if not self._retrying_frame:
                self._retrying_frame = True
                self._stats.frame_retries += 1
                self._emit_event(
                    "frame_retry",
                    timestamp_ms=frame.timestamp_ms,
                    frame_seq=frame.seq,
                    message=str(exc),
                    note="单帧处理异常，隔离重试一次",
                )
                try:
                    self._handle_frame(frame)
                finally:
                    self._retrying_frame = False
                return
            self._stats.frames_failed += 1
            codes = capability_codes + ["E_DECODE"]
            result = FrameResult(
                frame_seq=frame.seq,
                timestamp_ms=frame.timestamp_ms,
                phase=self.phase.value,
                status="failed",
                committed=False,
                codec=profile.name,
                width=self.negotiation.width,
                height=self.negotiation.height,
                frame_sha256=self._last_image_sha,
                wire_sha256="",
                packets_sent=0,
                packets_received=0,
                fragments_total=0,
                fragments_received=0,
                retransmissions=0,
                missing_fragments=[],
                codes=codes,
                message=f"单帧处理失败，可安全重试且未污染其他帧: {exc}",
                fragment_order=[],
                fragment_status=[],
            )
            self._frame_results.append(result)
            self._emit_event(
                "frame_failed",
                timestamp_ms=frame.timestamp_ms,
                frame_seq=frame.seq,
                codes=codes,
                message=str(exc),
            )

    def _input_rejected(self, event: InputEvent, code: str, message: str) -> InputResult:
        self._stats.inputs_rejected += 1
        if code == "E_STATE":
            self._stats.phase_rejections += 1
        result = InputResult(
            input_seq=event.seq,
            timestamp_ms=event.timestamp_ms,
            phase=self.phase.value,
            event_type=event.kind,
            status="rejected",
            code=code,
            codes=[code],
            source_coords=(
                [event.x, event.y, event.to_x or 0, event.to_y or 0]
                if event.kind == "drag"
                else [event.x, event.y]
            ),
            mapped_coords=[],
            clipped=False,
            clip_trace=[],
            desktop_digest=self._desktop.digest(),
            desktop_state=self._desktop.snapshot(),
            message=message,
        )
        self._input_results.append(result)
        self._emit_event(
            "input_rejected",
            timestamp_ms=event.timestamp_ms,
            input_seq=event.seq,
            code=code,
        )
        return result

    def _input_duplicate(self, event: InputEvent) -> InputResult:
        self._stats.inputs_duplicate += 1
        result = InputResult(
            input_seq=event.seq,
            timestamp_ms=event.timestamp_ms,
            phase=self.phase.value,
            event_type=event.kind,
            status="duplicate",
            code="E_DUP",
            codes=["E_DUP"],
            source_coords=[event.x, event.y],
            mapped_coords=[],
            clipped=False,
            clip_trace=[],
            desktop_digest=self._desktop.digest(),
            desktop_state=self._desktop.snapshot(),
            message="输入序号重复或过期，已幂等去重",
        )
        self._input_results.append(result)
        self._emit_event(
            "input_duplicate",
            timestamp_ms=event.timestamp_ms,
            input_seq=event.seq,
            code="E_DUP",
        )
        return result

    def _handle_input(self, event: InputEvent) -> None:
        if self.phase != Phase.MIRRORING:
            self._input_rejected(
                event,
                "E_STATE",
                "输入在非镜像中阶段到达，未改变桌面状态",
            )
            return
        if (
            event.seq in self._seen_input_seqs
            or event.seq <= self._last_replayed_seq
        ):
            self._input_duplicate(event)
            return
        envelope = InputEnvelope(self.sample.session_id, event)
        try:
            echoed = self._wire_input(envelope.to_bytes())
            decoded = InputEnvelope.from_bytes(
                echoed, expected_session_id=self.sample.session_id
            )
            mapped, trace = self._desktop.apply(decoded.event)
        except WorkbenchError as exc:
            self._input_rejected(event, exc.code, exc.message)
            return
        clipped = any(
            item.get("reason") in {"below_min", "above_max"} for item in trace
        )
        codes: list[str] = []
        if event.seq in self.config.anomaly.duplicate_inputs:
            codes.append("E_DUP")
            self._stats.inputs_duplicate += 1
            self._emit_event(
                "input_duplicate",
                timestamp_ms=event.timestamp_ms,
                input_seq=event.seq,
                code="E_DUP",
                note="异常注入：成功重放后再次回传同一序号",
            )
        self._seen_input_seqs.add(event.seq)
        self._last_replayed_seq = max(self._last_replayed_seq, event.seq)
        self._stats.inputs_replayed += 1
        result = InputResult(
            input_seq=event.seq,
            timestamp_ms=event.timestamp_ms,
            phase=self.phase.value,
            event_type=event.kind,
            status="replayed",
            code=codes[0] if codes else None,
            codes=codes,
            source_coords=(
                [event.x, event.y, event.to_x or 0, event.to_y or 0]
                if event.kind == "drag"
                else [event.x, event.y]
            ),
            mapped_coords=mapped,
            clipped=clipped,
            clip_trace=trace,
            desktop_digest=self._desktop.digest(),
            desktop_state=self._desktop.snapshot(),
        )
        self._input_results.append(result)
        self._emit_event(
            "input_replayed",
            timestamp_ms=event.timestamp_ms,
            input_seq=event.seq,
            event_type=event.kind,
            mapped_coords=mapped,
            clipped=clipped,
            desktop_digest=result.desktop_digest,
        )

    def step(self) -> bool:
        with self._lock:
            if not self._started:
                self.start()
            if self._timeline_index >= len(self._timeline):
                return False
            item = self._timeline[self._timeline_index]
            self._force_checkpoint = False
            if item.kind == "phase":
                action = self.sample.phase_script[item.payload_index]
                self._handle_phase_action(action.action, action.timestamp_ms)
            elif item.kind == "frame":
                self._handle_frame(self.sample.frames[item.payload_index])
            elif item.kind == "input":
                self._handle_input(self.sample.inputs[item.payload_index])
            else:
                raise SchemaError(f"未知时间线类型 {item.kind}")
            self._timeline_index += 1
            periodic = (
                item.kind == "frame"
                and self._stats.frames_committed > 0
                and self.config.checkpoint_every > 0
                and self._stats.frames_committed % self.config.checkpoint_every == 0
            )
            if (
                self.config.enable_checkpoints
                and self.checkpoint_manager is not None
                and (periodic or self._force_checkpoint)
            ):
                self.save_checkpoint()
            return True

    def run_to_end(self, max_steps: int = 10000) -> None:
        with self._lock:
            if not self._started:
                self.start()
            count = 0
            while self._timeline_index < len(self._timeline):
                self.step()
                count += 1
                if count > max_steps:
                    raise RuntimeError("时间线推进超过安全步数上限")

    def run_until_committed(self, frame_count: int) -> None:
        with self._lock:
            if not self._started:
                self.start()
            while (
                self._timeline_index < len(self._timeline)
                and self._stats.frames_committed < frame_count
            ):
                self.step()

    def pause(self) -> PhaseEvent:
        with self._lock:
            if self.phase != Phase.MIRRORING:
                raise StateError(
                    "只有镜像中阶段可以暂停", self.phase.value
                )
            event = self._transition(Phase.PAUSED, 0, "pause")
            self.save_checkpoint()
            return event

    def resume(self) -> PhaseEvent:
        with self._lock:
            if self.phase != Phase.PAUSED:
                raise StateError(
                    "只有暂停阶段可以恢复", self.phase.value
                )
            event = self._transition(Phase.MIRRORING, 0, "resume")
            self.save_checkpoint()
            return event

    def trigger_reconnect(self) -> list[PhaseEvent]:
        with self._lock:
            if self.phase != Phase.MIRRORING:
                raise StateError(
                    "只有镜像中阶段可以触发重连", self.phase.value
                )
            first = self._transition(Phase.RECONNECTING, 0, "manual_reconnect")
            second = self._transition(
                Phase.MIRRORING, 0, "manual_reconnect_complete"
            )
            self.save_checkpoint()
            return [first, second]

    def set_runtime_tier(self, tier: str) -> None:
        with self._lock:
            if tier not in self._allowed_tiers:
                raise CapabilityChangeError(
                    "运行期编码档位不在协商交集内",
                    items=[f"codec:{tier}"],
                    allowed=list(self._allowed_tiers),
                )
            self._runtime_tier = tier
            self._emit_event(
                "runtime_tier_changed",
                timestamp_ms=self._timeline[
                    min(self._timeline_index, len(self._timeline) - 1)
                ].timestamp_ms,
                codec=tier,
                note="仅影响尚未提交的帧",
            )

    def set_retry_limit(self, limit: int) -> None:
        with self._lock:
            if not 0 <= int(limit) <= 16:
                raise SchemaError("retry_limit 必须在 0..16")
            self._retry_limit = int(limit)
            self._emit_event(
                "retry_limit_changed",
                timestamp_ms=0,
                retry_limit=self._retry_limit,
                note="仅影响尚未提交的帧",
            )

    def _checkpoint_payload(self) -> dict[str, Any]:
        last_image_b64 = ""
        last_image_sha = ""
        if self._last_image is not None:
            raw = self._last_image.tobytes(order="C")
            last_image_sha = hashlib.sha256(raw).hexdigest()
            last_image_b64 = base64.b64encode(
                zlib.compress(raw, 6)
            ).decode("ascii")
        last_fragment_state: dict[str, Any] = {}
        if self._frame_results:
            last = self._frame_results[-1]
            last_fragment_state = {
                "frame_seq": last.frame_seq,
                "fragment_status": list(last.fragment_status),
                "received": last.fragments_received,
                "total": last.fragments_total,
                "missing": list(last.missing_fragments),
                "pending": [],
            }
        return {
            "payload_version": 1,
            "session_id": self.sample.session_id,
            "protocol_version": PROTOCOL_VERSION,
            "content_hash": self.sample.content_hash,
            "capabilities_hash": self.negotiation.capabilities_hash,
            "phase": self.phase.value,
            "timeline_index": self._timeline_index,
            "negotiation": self.negotiation.to_dict(),
            "runtime_tier": self._runtime_tier,
            "retry_limit": self._retry_limit,
            "max_parallelism": self.config.max_parallelism,
            "per_thread_buffer": self.config.per_thread_buffer,
            "frame_results": [item.to_dict() for item in self._frame_results],
            "input_results": [item.to_dict() for item in self._input_results],
            "phase_events": [item.to_dict() for item in self._phase_events],
            "events": copy.deepcopy(self._events),
            "stats": self._stats.to_dict(),
            "desktop": self._desktop.to_checkpoint(),
            "seen_input_seqs": sorted(self._seen_input_seqs),
            "last_replayed_seq": self._last_replayed_seq,
            "last_image_sha256": last_image_sha,
            "last_image_zlib_b64": last_image_b64,
            "fragment_state": last_fragment_state,
            "checkpoint_count": self._checkpoint_count + 1,
            "capability_event_emitted": self._capability_event_emitted,
        }

    def save_checkpoint(self) -> None:
        with self._lock:
            if self.checkpoint_manager is None:
                return
            payload = self._checkpoint_payload()
            self.checkpoint_manager.save(payload)
            self._checkpoint_count += 1

    def restore(self, payload: dict[str, Any]) -> bool:
        with self._lock:
            try:
                if int(payload.get("payload_version", -1)) != 1:
                    return False
                if payload.get("content_hash") != self.sample.content_hash:
                    return False
                if payload.get("capabilities_hash") != self.negotiation.capabilities_hash:
                    return False
                if payload.get("protocol_version") != PROTOCOL_VERSION:
                    return False
                if payload.get("session_id") != self.sample.session_id:
                    return False
                stored_negotiation = NegotiationResult.from_dict(payload["negotiation"])
                if stored_negotiation != self.negotiation:
                    return False
                restored_phase = Phase(payload["phase"])
                restored_timeline_index = int(payload["timeline_index"])
                if restored_timeline_index < 0 or restored_timeline_index > len(
                    self._timeline
                ):
                    return False
                restored_tier = str(payload["runtime_tier"])
                if restored_tier not in self._allowed_tiers:
                    return False
                restored_retry_limit = int(payload.get("retry_limit", 2))
                if not 0 <= restored_retry_limit <= 16:
                    return False
                restored_frames = [
                    FrameResult.from_dict(item)
                    for item in payload.get("frame_results", [])
                ]
                restored_inputs = [
                    InputResult.from_dict(item)
                    for item in payload.get("input_results", [])
                ]
                restored_phase_events = [
                    PhaseEvent.from_dict(item)
                    for item in payload.get("phase_events", [])
                ]
                restored_events = copy.deepcopy(payload.get("events", []))
                restored_stats = SessionStats.from_dict(payload["stats"])
                restored_desktop = DesktopModel.from_checkpoint(payload["desktop"])
                restored_seen_inputs = {
                    int(item) for item in payload.get("seen_input_seqs", [])
                }
                restored_last_input_seq = int(
                    payload.get("last_replayed_seq", 0)
                )
                restored_capability_marker = bool(
                    payload.get("capability_event_emitted", False)
                )
                restored_image: np.ndarray | None = None
                restored_image_sha = ""
                encoded_image = payload.get("last_image_zlib_b64", "")
                if encoded_image:
                    raw = zlib.decompress(base64.b64decode(encoded_image))
                    expected_bytes = (
                        self.negotiation.width * self.negotiation.height * 4
                    )
                    if len(raw) != expected_bytes:
                        return False
                    actual_sha = hashlib.sha256(raw).hexdigest()
                    if actual_sha != payload.get("last_image_sha256"):
                        return False
                    restored_image = np.frombuffer(raw, dtype=np.uint8).reshape(
                        self.negotiation.height,
                        self.negotiation.width,
                        4,
                    ).copy()
                    restored_image_sha = image_digest(restored_image)
                restored_checkpoint_count = int(
                    payload.get("checkpoint_count", 0)
                )
                self.phase = restored_phase
                self._timeline_index = restored_timeline_index
                self._runtime_tier = restored_tier
                self._retry_limit = restored_retry_limit
                self._frame_results = restored_frames
                self._input_results = restored_inputs
                self._phase_events = restored_phase_events
                self._events = restored_events
                self._stats = restored_stats
                self._desktop = restored_desktop
                self._seen_input_seqs = restored_seen_inputs
                self._last_replayed_seq = restored_last_input_seq
                self._capability_event_emitted = restored_capability_marker
                self._last_image = restored_image
                self._last_image_sha = restored_image_sha
                self._checkpoint_count = restored_checkpoint_count
                self._started = True
                return True
            except (
                KeyError,
                TypeError,
                ValueError,
                binascii.Error,
                zlib.error,
                SchemaError,
            ):
                return False

    def to_run_result(self, mode: str) -> RunResult:
        with self._lock:
            return RunResult(
                mode=mode,
                session_id=self.sample.session_id,
                content_hash=self.sample.content_hash,
                capabilities_hash=self.negotiation.capabilities_hash,
                negotiation=self.negotiation,
                final_phase=self.phase.value,
                frames=copy.deepcopy(self._frame_results),
                inputs=copy.deepcopy(self._input_results),
                phase_events=copy.deepcopy(self._phase_events),
                stats=copy.deepcopy(self._stats),
                desktop_state=self._desktop.snapshot(),
                events=copy.deepcopy(self._events),
                checkpoint_count=self._checkpoint_count,
            )

    def events_since(self, index: int) -> list[dict[str, Any]]:
        with self._lock:
            return copy.deepcopy(self._events[max(0, int(index)) :])

    @property
    def event_count(self) -> int:
        with self._lock:
            return len(self._events)

    def ui_snapshot(self) -> dict[str, Any]:
        with self._lock:
            return {
                "session_id": self.sample.session_id,
                "session_name": self.sample.session_name,
                "phase": self.phase.value,
                "phase_label": self.phase.label,
                "started": self._started,
                "finished": self.finished,
                "timeline_index": self._timeline_index,
                "timeline_total": len(self._timeline),
                "total_frames": len(self.sample.frames),
                "total_inputs": len(self.sample.inputs),
                "stats": self._stats.to_dict(),
                "negotiation": self.negotiation.to_dict(),
                "runtime_tier": self._runtime_tier,
                "retry_limit": self._retry_limit,
                "checkpoint_count": self._checkpoint_count,
                "anomaly": self.config.anomaly.to_dict(),
                "frames": [item.to_dict() for item in self._frame_results[-20:]],
                "inputs": [item.to_dict() for item in self._input_results[-20:]],
                "phase_events": [
                    item.to_dict() for item in self._phase_events[-20:]
                ],
                "events": copy.deepcopy(self._events[-80:]),
                "desktop": self._desktop.snapshot(),
                "last_frame_sha256": self._last_image_sha,
            }

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self._executor.shutdown(wait=True)
        self.transport.close()

    def __enter__(self) -> "SessionEngine":
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()
