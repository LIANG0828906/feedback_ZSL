from __future__ import annotations

import json
import socket
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable

from .engine import advance_state
from .errors import E_RESOURCE, E_SEQ, KernelError, KernelIssue
from .models import (
    AuthoritativeState,
    MatchContext,
    state_differences,
)
from .protocol import DecodedFrame, decode_frame
from .util import MAX_FRAME_BYTES, canonical_json_bytes, parse_json_payload
from .util import recv_line


@dataclass(frozen=True)
class CorrectionRecord:
    tick: int
    confirmed_sequence: int
    unit_id: str
    field: str
    predicted: int | bool
    authoritative: int | bool
    delta: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "tick": self.tick,
            "confirmed_sequence": self.confirmed_sequence,
            "unit_id": self.unit_id,
            "field": self.field,
            "predicted": self.predicted,
            "authoritative": self.authoritative,
            "delta": self.delta,
        }


@dataclass
class FeedResult:
    applied: bool
    applied_sequence: int | None
    issues: list[KernelIssue] = field(default_factory=list)
    expected_sequence: int = 0
    correction_count: int = 0
    confirmed_hash: str = ""

    def as_dict(self) -> dict[str, Any]:
        return {
            "applied": self.applied,
            "applied_sequence": self.applied_sequence,
            "expected_sequence": self.expected_sequence,
            "correction_count": self.correction_count,
            "confirmed_hash": self.confirmed_hash,
            "issues": [issue.as_dict() for issue in self.issues],
        }


class PredictiveClient:
    """Client-side prediction with authoritative rollback and input replay."""

    def __init__(
        self,
        *,
        version: int,
        context: MatchContext,
        initial_state: AuthoritativeState,
        inputs_by_tick: dict[int, list[dict[str, Any]]],
        max_pending_frames: int = 64,
    ) -> None:
        self.version = version
        self.context = context
        self.initial_state = initial_state.clone()
        self.confirmed_state = initial_state.clone()
        self.predicted_state = initial_state.clone()
        self.confirmed_sequence = initial_state.sequence
        self._has_authority_frame = False
        self.inputs_by_tick = inputs_by_tick
        self.max_pending_frames = max_pending_frames
        self._pending: dict[int, tuple[bytes, int]] = {}
        self._pending_bytes = 0
        self.prediction_history: dict[int, AuthoritativeState] = {
            initial_state.tick: initial_state.clone()
        }
        self.confirmed_history: dict[int, AuthoritativeState] = {
            initial_state.tick: initial_state.clone()
        }
        self.prediction_horizon = initial_state.tick
        self.corrections: list[CorrectionRecord] = []
        self.issues: list[KernelIssue] = []
        self.last_decoded_frame: DecodedFrame | None = None
        self._gap_reported_for: int | None = None

    @property
    def expected_sequence(self) -> int:
        return self.confirmed_sequence + 1

    def predict_to(self, target_tick: int) -> AuthoritativeState:
        if target_tick < self.predicted_state.tick:
            self.predicted_state = self.confirmed_state.clone()
            self.prediction_history = {
                self.confirmed_state.tick: self.confirmed_state.clone()
            }
        while self.predicted_state.tick < target_tick:
            next_tick = self.predicted_state.tick + 1
            intents = self.inputs_by_tick.get(next_tick, [])
            self.predicted_state = advance_state(
                self.predicted_state,
                self.context,
                intents,
            )
            self.prediction_history[next_tick] = self.predicted_state.clone()
        self.prediction_horizon = max(self.prediction_horizon, target_tick)
        return self.predicted_state.clone()

    def _frame_metadata(self, payload: bytes | dict[str, Any]) -> tuple[int, int, int]:
        frame = parse_json_payload(payload)
        version = frame.get("protocol_version")
        sequence = frame.get("sequence")
        if not isinstance(version, int) or isinstance(version, bool):
            raise KernelError(
                "E_VER",
                "protocol_version missing or not an integer",
                path="$.protocol_version",
                source_version=version,
            )
        if not isinstance(sequence, int) or isinstance(sequence, bool):
            raise KernelError(
                "E_VER",
                "sequence missing or not an integer",
                path="$.sequence",
                source_version=version,
            )
        return version, sequence, len(canonical_json_bytes(frame))

    def _record_issue(self, issue: KernelIssue) -> None:
        self.issues.append(issue)
        if len(self.issues) > 256:
            del self.issues[: len(self.issues) - 256]

    def _record_error(self, error: KernelError) -> KernelIssue:
        self._record_issue(error.issue)
        return error.issue

    def _apply_contiguous(self, payload: bytes, sequence: int) -> FeedResult:
        is_initial_authority = not self._has_authority_frame and sequence == 0
        if not is_initial_authority and sequence != self.confirmed_sequence + 1:
            raise KernelError(
                E_SEQ,
                "attempted to apply a non-contiguous sequence",
                path="$.sequence",
                details={
                    "expected": self.confirmed_sequence + 1,
                    "received": sequence,
                },
            )
        use_base = None if is_initial_authority else self.confirmed_state
        decoded = decode_frame(payload, base=use_base, context=self.context)
        if decoded.source_sequence != sequence:
            raise KernelError(
                E_SEQ,
                "decoded sequence differs from transport sequence",
                path="$.sequence",
                source_version=decoded.source_version,
                details={"transport": sequence, "decoded": decoded.source_sequence},
            )
        predicted_at_tick = self.prediction_history.get(decoded.state.tick)
        correction_start = len(self.corrections)
        if predicted_at_tick is not None:
            for difference in state_differences(predicted_at_tick, decoded.state):
                if difference["field"] == "unit":
                    continue
                self.corrections.append(
                    CorrectionRecord(
                        tick=decoded.state.tick,
                        confirmed_sequence=sequence,
                        unit_id=str(difference["unit_id"]),
                        field=str(difference["field"]),
                        predicted=difference["predicted"],
                        authoritative=difference["authoritative"],
                        delta=int(difference["delta"]),
                    )
                )

        self.confirmed_state = decoded.state.clone()
        self.confirmed_sequence = decoded.source_sequence
        self._has_authority_frame = True
        self.confirmed_history[decoded.state.tick] = decoded.state.clone()
        self.last_decoded_frame = decoded

        previous_horizon = max(self.prediction_horizon, decoded.state.tick)
        self.predicted_state = decoded.state.clone()
        rebuilt_history = {decoded.state.tick: decoded.state.clone()}
        while self.predicted_state.tick < previous_horizon:
            next_tick = self.predicted_state.tick + 1
            self.predicted_state = advance_state(
                self.predicted_state,
                self.context,
                self.inputs_by_tick.get(next_tick, []),
            )
            rebuilt_history[next_tick] = self.predicted_state.clone()
        self.prediction_history = rebuilt_history
        self.prediction_horizon = max(previous_horizon, self.predicted_state.tick)

        return FeedResult(
            applied=True,
            applied_sequence=sequence,
            expected_sequence=self.expected_sequence,
            correction_count=len(self.corrections) - correction_start,
            confirmed_hash=self.confirmed_state.snapshot_hash,
        )

    def _drain_pending(self) -> list[KernelIssue]:
        issues: list[KernelIssue] = []
        while self.expected_sequence in self._pending:
            payload, size = self._pending.pop(self.expected_sequence)
            self._pending_bytes = max(0, self._pending_bytes - size)
            try:
                result = self._apply_contiguous(payload, self.expected_sequence - 1 + 1)
            except KernelError as error:
                issues.append(self._record_error(error))
                break
            issues.extend(result.issues)
        return issues

    def feed(self, payload: bytes | str | dict[str, Any]) -> FeedResult:
        try:
            source_version, sequence, payload_size = self._frame_metadata(payload)
        except KernelError as error:
            issue = self._record_error(error)
            return FeedResult(
                applied=False,
                applied_sequence=None,
                issues=[issue],
                expected_sequence=self.expected_sequence,
                confirmed_hash=self.confirmed_state.snapshot_hash,
            )
        if source_version != self.version:
            issue = self._record_error(
                KernelError(
                    "E_VER",
                    "client protocol version differs from frame protocol version",
                    path="$.protocol_version",
                    source_version=source_version,
                    details={"client_version": self.version},
                )
            )
            return FeedResult(
                applied=False,
                applied_sequence=None,
                issues=[issue],
                expected_sequence=self.expected_sequence,
                confirmed_hash=self.confirmed_state.snapshot_hash,
            )
        if isinstance(payload, bytes):
            canonical = payload
        elif isinstance(payload, str):
            canonical = payload.encode("utf-8")
        else:
            canonical = canonical_json_bytes(payload)
        if self._has_authority_frame and sequence <= self.confirmed_sequence:
            issue = self._record_error(
                KernelError(
                    E_SEQ,
                    "duplicate or reversed sequence was ignored without mutating state",
                    path="$.sequence",
                    source_version=source_version,
                    details={
                        "confirmed_sequence": self.confirmed_sequence,
                        "received_sequence": sequence,
                    },
                )
            )
            return FeedResult(
                applied=False,
                applied_sequence=None,
                issues=[issue],
                expected_sequence=self.expected_sequence,
                confirmed_hash=self.confirmed_state.snapshot_hash,
            )
        if sequence > self.confirmed_sequence + 1:
            if sequence not in self._pending:
                if (
                    len(self._pending) >= self.max_pending_frames
                    or self._pending_bytes + payload_size > MAX_FRAME_BYTES
                ):
                    issue = self._record_error(
                        KernelError(
                            E_RESOURCE,
                            "client pending-frame buffer limit exceeded",
                            path="$.sequence",
                            source_version=source_version,
                            details={
                                "pending_frames": len(self._pending),
                                "pending_bytes": self._pending_bytes + payload_size,
                                "max_pending_frames": self.max_pending_frames,
                            },
                        )
                    )
                    return FeedResult(
                        applied=False,
                        applied_sequence=None,
                        issues=[issue],
                        expected_sequence=self.expected_sequence,
                        confirmed_hash=self.confirmed_state.snapshot_hash,
                    )
                self._pending[sequence] = (canonical, payload_size)
                self._pending_bytes += payload_size
            issue = KernelError(
                E_SEQ,
                "sequence gap detected; frame buffered and confirmed state preserved",
                path="$.sequence",
                source_version=source_version,
                details={
                    "expected": self.expected_sequence,
                    "received": sequence,
                    "buffered": True,
                },
            ).issue
            if self._gap_reported_for != self.expected_sequence:
                self._gap_reported_for = self.expected_sequence
                self._record_issue(issue)
            return FeedResult(
                applied=False,
                applied_sequence=None,
                issues=[issue],
                expected_sequence=self.expected_sequence,
                confirmed_hash=self.confirmed_state.snapshot_hash,
            )
        try:
            result = self._apply_contiguous(canonical, sequence)
        except KernelError as error:
            issue = self._record_error(error)
            return FeedResult(
                applied=False,
                applied_sequence=None,
                issues=[issue],
                expected_sequence=self.expected_sequence,
                confirmed_hash=self.confirmed_state.snapshot_hash,
            )
        result.issues.extend(self._drain_pending())
        result.expected_sequence = self.expected_sequence
        result.confirmed_hash = self.confirmed_state.snapshot_hash
        return result

    def report(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "confirmed_sequence": self.confirmed_sequence,
            "tick": self.confirmed_state.tick,
            "state_hash": self.confirmed_state.snapshot_hash,
            "corrections": len(self.corrections),
            "issues": [issue.as_dict() for issue in self.issues],
        }


class LoopbackClient:
    """Threaded newline-delimited JSON client for the local sync service."""

    def __init__(
        self,
        *,
        host: str,
        port: int,
        version: int,
        context: MatchContext,
        initial_state: AuthoritativeState,
        inputs_by_tick: dict[int, list[dict[str, Any]]],
        timeout: float = 8.0,
    ) -> None:
        self.host = host
        self.port = port
        self.predictor = PredictiveClient(
            version=version,
            context=context,
            initial_state=initial_state,
            inputs_by_tick=inputs_by_tick,
        )
        self.timeout = timeout
        self._sock: socket.socket | None = None
        self._recv_buffer = bytearray()
        self._send_lock = threading.Lock()
        self._done = threading.Event()
        self._stopped = threading.Event()
        self._thread: threading.Thread | None = None
        self._target_sequence: int | None = None
        self._last_resync_request: int | None = None
        self._target_reached_at: float | None = None
        self.echoes: list[dict[str, Any]] = []
        self.transport_errors: list[str] = []
        self.received_frames = 0

    @property
    def confirmed_state(self) -> AuthoritativeState:
        return self.predictor.confirmed_state

    def _send(self, value: dict[str, Any]) -> None:
        if self._sock is None:
            raise ConnectionError("client socket is not connected")
        data = canonical_json_bytes(value) + b"\n"
        with self._send_lock:
            self._sock.sendall(data)

    def _request_resync(self) -> None:
        expected = self.predictor.expected_sequence
        if self._last_resync_request == expected:
            return
        self._last_resync_request = expected
        self._send(
            {
                "type": "resync",
                "from_sequence": expected,
                "version": self.predictor.version,
            }
        )

    def connect(self) -> "LoopbackClient":
        sock = socket.create_connection((self.host, self.port), timeout=3.0)
        sock.settimeout(0.2)
        self._sock = sock
        self._send(
            {
                "type": "hello",
                "version": self.predictor.version,
                "match_id": self.predictor.context.match_id,
            }
        )
        self._thread = threading.Thread(target=self._receive_loop, daemon=True)
        self._thread.start()
        return self

    def _receive_loop(self) -> None:
        assert self._sock is not None
        while not self._stopped.is_set():
            line, status = recv_line(
                self._sock,
                self._recv_buffer,
                timeout=0.2,
                max_bytes=MAX_FRAME_BYTES,
            )
            if status == "timeout":
                if self._target_sequence is not None and (
                    self.predictor.confirmed_sequence >= self._target_sequence
                ):
                    if self._target_reached_at is None:
                        self._target_reached_at = time.monotonic()
                    if time.monotonic() - self._target_reached_at >= 0.5:
                        self._done.set()
                        break
                continue
            if status == "closed":
                break
            if status != "ok" or line is None:
                self.transport_errors.append(f"server frame read failed: {status}")
                break
            try:
                message = json.loads(line.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                self.transport_errors.append(f"invalid server JSON: {exc}")
                continue
            kind = message.get("type") if isinstance(message, dict) else None
            is_sync_frame = isinstance(message, dict) and (
                kind == "sync" or ("protocol_version" in message and kind is None)
            )
            if is_sync_frame:
                self.received_frames += 1
                result = self.predictor.feed(message)
                if result.applied:
                    self._send(
                        {
                            "type": "report",
                            "version": self.predictor.version,
                            "sequence": result.applied_sequence,
                            "state_hash": result.confirmed_hash,
                            "tick": self.predictor.confirmed_state.tick,
                        }
                    )
                for issue in result.issues:
                    if issue.code == E_SEQ:
                        self._request_resync()
                if (
                    self._target_sequence is not None
                    and self.predictor.confirmed_sequence >= self._target_sequence
                ):
                    if self._target_reached_at is None:
                        self._target_reached_at = time.monotonic()
            elif kind == "complete":
                last_sequence = message.get("last_sequence")
                if isinstance(last_sequence, int):
                    self._target_sequence = last_sequence
                    if self.predictor.confirmed_sequence >= last_sequence:
                        if self._target_reached_at is None:
                            self._target_reached_at = time.monotonic()
                if self.predictor.confirmed_sequence < self.predictor.expected_sequence - 1:
                    try:
                        self._request_resync()
                    except OSError:
                        pass
            elif kind == "echo":
                self.echoes.append(message)
                if (
                    self._target_sequence is not None
                    and isinstance(message.get("sequence"), int)
                    and message["sequence"] >= self._target_sequence - 1
                ):
                    self._done.set()
                    break
            elif kind == "error":
                self.transport_errors.append(str(message))

    def wait(self, timeout: float | None = None) -> bool:
        return self._done.wait(self.timeout if timeout is None else timeout)

    def close(self) -> None:
        self._stopped.set()
        if self._sock is not None:
            try:
                self._sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                self._sock.close()
            except OSError:
                pass
        if self._thread is not None:
            self._thread.join(timeout=2.0)

