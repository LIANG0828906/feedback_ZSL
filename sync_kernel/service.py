from __future__ import annotations

import queue
import socket
import threading
from dataclasses import dataclass, field
from typing import Any

from .util import MAX_FRAME_BYTES, canonical_json_bytes, recv_line


@dataclass(frozen=True)
class AnomalyPlan:
    drop_sequences: tuple[int, ...] = ()
    duplicate_sequences: tuple[int, ...] = ()
    reorder_sequences: tuple[int, ...] = ()

    @classmethod
    def default(cls, last_sequence: int) -> "AnomalyPlan":
        candidates_drop = (7,) if last_sequence >= 7 else ()
        candidates_duplicate = (13,) if last_sequence >= 13 else ()
        candidates_reorder = (21,) if last_sequence >= 22 else ()
        return cls(
            drop_sequences=candidates_drop,
            duplicate_sequences=candidates_duplicate,
            reorder_sequences=candidates_reorder,
        )


@dataclass
class ServerStats:
    accepted_clients: int = 0
    frames_sent: int = 0
    reports_received: int = 0
    echoes_sent: int = 0
    resync_requests: int = 0
    dropped: int = 0
    duplicated: int = 0
    reordered: int = 0
    dropped_events: int = 0


class LoopbackServer:
    """Local TCP sync service with deterministic fault injection."""

    def __init__(
        self,
        *,
        frames_by_version: dict[int, list[dict[str, Any]]],
        anomaly_plans: dict[int, AnomalyPlan] | None = None,
        host: str = "127.0.0.1",
        port: int = 0,
        max_clients: int = 8,
        event_queue_size: int = 512,
    ) -> None:
        self.host = host
        self.requested_port = port
        self.frames_by_version = {
            int(version): list(frames)
            for version, frames in frames_by_version.items()
        }
        self.payloads_by_version = {
            version: [canonical_json_bytes(frame) for frame in frames]
            for version, frames in self.frames_by_version.items()
        }
        self.anomaly_plans = anomaly_plans or {
            version: AnomalyPlan.default(len(frames) - 1)
            for version, frames in self.frames_by_version.items()
        }
        self.max_clients = max_clients
        self.stats = ServerStats()
        self.report_events: queue.Queue[dict[str, Any]] = queue.Queue(
            maxsize=event_queue_size
        )
        self.reports: list[dict[str, Any]] = []
        self.reports_by_version: dict[int, list[dict[str, Any]]] = {
            version: [] for version in self.frames_by_version
        }
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._sock: socket.socket | None = None
        self._accept_thread: threading.Thread | None = None
        self._handlers: list[threading.Thread] = []
        self._active_clients = 0
        self.port = 0

    @property
    def url(self) -> str:
        return f"tcp://{self.host}:{self.port}"

    def start(self) -> "LoopbackServer":
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind((self.host, self.requested_port))
        sock.listen(self.max_clients)
        sock.settimeout(0.2)
        self._sock = sock
        self.port = int(sock.getsockname()[1])
        self._accept_thread = threading.Thread(target=self._accept_loop, daemon=True)
        self._accept_thread.start()
        return self

    def _accept_loop(self) -> None:
        assert self._sock is not None
        while not self._stop.is_set():
            try:
                client, _address = self._sock.accept()
            except (socket.timeout, TimeoutError):
                continue
            except OSError:
                break
            with self._lock:
                if self._active_clients >= self.max_clients:
                    try:
                        client.sendall(
                            canonical_json_bytes(
                                {"type": "error", "code": "E_RESOURCE", "message": "server full"}
                            )
                            + b"\n"
                        )
                    finally:
                        client.close()
                    continue
                self._active_clients += 1
                self.stats.accepted_clients += 1
            handler = threading.Thread(
                target=self._handle_client,
                args=(client,),
                daemon=True,
            )
            with self._lock:
                self._handlers.append(handler)
            handler.start()

    def _initial_order(self, version: int) -> list[int]:
        count = len(self.frames_by_version[version])
        plan = self.anomaly_plans.get(version, AnomalyPlan())
        drops = set(plan.drop_sequences)
        duplicates = set(plan.duplicate_sequences)
        reorders = set(plan.reorder_sequences)
        ordered: list[int] = []
        sequence = 0
        skipped: set[int] = set()
        while sequence < count:
            if sequence in skipped:
                sequence += 1
                continue
            if sequence in drops:
                sequence += 1
                continue
            if sequence in reorders and sequence + 1 < count:
                ordered.extend([sequence + 1, sequence])
                skipped.add(sequence + 1)
                sequence += 2
                continue
            ordered.append(sequence)
            sequence += 1
        expanded: list[int] = []
        for item in ordered:
            expanded.append(item)
            if item in duplicates:
                expanded.append(item)
        return expanded

    def _send_message(self, client: socket.socket, value: dict[str, Any]) -> bool:
        data = canonical_json_bytes(value) + b"\n"
        if len(data) > MAX_FRAME_BYTES + 1:
            return False
        try:
            client.sendall(data)
        except OSError:
            return False
        return True

    def _send_sync(self, client: socket.socket, version: int, sequence: int) -> bool:
        payload = self.payloads_by_version[version][sequence]
        if len(payload) > MAX_FRAME_BYTES:
            return False
        try:
            client.sendall(payload + b"\n")
        except OSError:
            return False
        with self._lock:
            self.stats.frames_sent += 1
        return True

    def _handle_client(self, client: socket.socket) -> None:
        client.settimeout(0.2)
        recv_buffer = bytearray()
        version: int | None = None
        try:
            while not self._stop.is_set():
                line, status = recv_line(
                    client,
                    recv_buffer,
                    timeout=0.2,
                    max_bytes=MAX_FRAME_BYTES,
                )
                if status == "timeout":
                    continue
                if status == "closed":
                    break
                if status != "ok" or line is None:
                    self._send_message(
                        client,
                        {
                            "type": "error",
                            "code": "E_RESOURCE",
                            "message": f"line read failed: {status}",
                        },
                    )
                    break
                try:
                    import json

                    message = json.loads(line.decode("utf-8"))
                except Exception:
                    self._send_message(
                        client,
                        {"type": "error", "code": "E_DECODE", "message": "invalid JSON"},
                    )
                    continue
                kind = message.get("type")
                if kind == "hello":
                    requested = message.get("version")
                    if not isinstance(requested, int) or requested not in self.payloads_by_version:
                        self._send_message(
                            client,
                            {
                                "type": "error",
                                "code": "E_VER",
                                "message": "unsupported protocol version",
                            },
                        )
                        break
                    version = requested
                    plan = self.anomaly_plans.get(
                        version, AnomalyPlan.default(len(self.frames_by_version[version]) - 1)
                    )
                    for sequence in self._initial_order(version):
                        if not self._send_sync(client, version, sequence):
                            break
                    with self._lock:
                        self.stats.dropped += len(plan.drop_sequences)
                        self.stats.duplicated += len(plan.duplicate_sequences)
                        self.stats.reordered += len(plan.reorder_sequences)
                    self._send_message(
                        client,
                        {
                            "type": "complete",
                            "version": version,
                            "last_sequence": len(self.frames_by_version[version]) - 1,
                        },
                    )
                elif kind == "resync" and version is not None:
                    if not isinstance(message.get("from_sequence"), int):
                        continue
                    start = max(0, int(message["from_sequence"]))
                    with self._lock:
                        self.stats.resync_requests += 1
                    for sequence in range(start, len(self.frames_by_version[version])):
                        if not self._send_sync(client, version, sequence):
                            break
                    self._send_message(
                        client,
                        {
                            "type": "complete",
                            "version": version,
                            "last_sequence": len(self.frames_by_version[version]) - 1,
                        },
                    )
                elif kind == "report" and version is not None:
                    record = {
                        "version": version,
                        "sequence": message.get("sequence"),
                        "state_hash": message.get("state_hash"),
                        "tick": message.get("tick"),
                    }
                    with self._lock:
                        self.reports.append(record)
                        self.reports_by_version[version].append(record)
                        self.stats.reports_received += 1
                    try:
                        self.report_events.put_nowait(record)
                    except queue.Full:
                        with self._lock:
                            self.stats.dropped_events += 1
                    if self._send_message(
                        client,
                        {
                            "type": "echo",
                            "sequence": record["sequence"],
                            "state_hash": record["state_hash"],
                        },
                    ):
                        with self._lock:
                            self.stats.echoes_sent += 1
        finally:
            try:
                client.close()
            except OSError:
                pass
            with self._lock:
                self._active_clients = max(0, self._active_clients - 1)

    def latest_report(self, version: int) -> dict[str, Any] | None:
        with self._lock:
            records = self.reports_by_version.get(version, [])
            return dict(records[-1]) if records else None

    def stop(self) -> None:
        self._stop.set()
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
        if self._accept_thread is not None:
            self._accept_thread.join(timeout=2.0)
        with self._lock:
            handlers = list(self._handlers)
        for handler in handlers:
            handler.join(timeout=2.0)

    def __enter__(self) -> "LoopbackServer":
        return self.start()

    def __exit__(self, *_args: Any) -> None:
        self.stop()

