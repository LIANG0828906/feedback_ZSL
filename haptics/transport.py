from __future__ import annotations

import json
import socket
import socketserver
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Iterable, Sequence

from .codec import decode_header
from .errors import ErrorCode, HapticError, Issue
from .models import Echo

_MAX_REQUEST_BYTES = 16 * 1024 * 1024


@dataclass(frozen=True)
class FaultPlan:
    """Deterministic first-attempt fault injection profile.

    Retry requests intentionally bypass all faults so one diagnostic packet can
    be recovered without changing any other packet's observation.
    """

    drops: tuple[int, ...] = ()
    duplicates: tuple[int, ...] = ()
    delays_ms: tuple[tuple[int, int], ...] = ()
    reorder_pairs: tuple[tuple[int, int], ...] = ()

    def delay_for(self, packet_seq: int) -> int:
        return dict(self.delays_ms).get(packet_seq, 0)

    @classmethod
    def sample(cls) -> "FaultPlan":
        return cls(
            drops=(3,),
            duplicates=(6,),
            delays_ms=((8, 120),),
            reorder_pairs=((9, 10),),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "drops": list(self.drops),
            "duplicates": list(self.duplicates),
            "delays_ms": [list(item) for item in self.delays_ms],
            "reorder_pairs": [list(item) for item in self.reorder_pairs],
        }


@dataclass(frozen=True)
class DispatchRecord:
    packet_seq: int
    channel: int
    data: bytes
    sent_at_ms: float


@dataclass
class _ServiceState:
    attempts: dict[tuple[str, int], int] = field(default_factory=dict)
    batch_counter: int = 0
    lock: threading.RLock = field(default_factory=threading.RLock)


class _ThreadingTCPServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True
    daemon_threads = True
    service: "MockDeviceService"


class _RequestHandler(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        try:
            raw = self.rfile.readline(_MAX_REQUEST_BYTES)
            if not raw:
                return
            request = json.loads(raw.decode("utf-8"))
            response = self.server.service._process_request(request)  # type: ignore[attr-defined]
            self.wfile.write(
                (json.dumps(response, ensure_ascii=False, sort_keys=True) + "\n").encode("utf-8")
            )
        except Exception as exc:  # A malformed local request gets a structured reply.
            reply = {
                "ok": False,
                "error": {
                    "code": ErrorCode.E_IO.value,
                    "message": f"mock device request failed: {exc}",
                },
            }
            try:
                self.wfile.write(
                    (json.dumps(reply, ensure_ascii=False, sort_keys=True) + "\n").encode("utf-8")
                )
            except OSError:
                pass


class MockDeviceService:
    """Local loopback simulated haptic device.

    The service uses deterministic logical processing times.  A wall-clock
    timestamp is also returned for operations/debugging, but all alignment and
    verification logic ignores it so repeated runs are byte-for-byte stable.
    """

    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 0,
        fault_plan: FaultPlan | None = None,
    ):
        self.host = host
        self.requested_port = port
        self.fault_plan = fault_plan or FaultPlan()
        self._state = _ServiceState()
        self._server: _ThreadingTCPServer | None = None
        self._thread: threading.Thread | None = None
        self._started = False

    @property
    def port(self) -> int:
        if self._server is None:
            return self.requested_port
        return int(self._server.server_address[1])

    @property
    def address(self) -> tuple[str, int]:
        return (self.host, self.port)

    def start(self) -> "MockDeviceService":
        if self._started:
            return self
        try:
            self._server = _ThreadingTCPServer((self.host, self.requested_port), _RequestHandler)
        except OSError as exc:
            raise HapticError(
                Issue(
                    ErrorCode.E_IO,
                    f"cannot bind mock device loopback port {self.host}:{self.requested_port}: {exc}",
                    field="port",
                    details={"host": self.host, "port": self.requested_port},
                )
            ) from exc
        self._server.service = self
        self._thread = threading.Thread(
            target=self._server.serve_forever,
            name="mock-haptic-device",
            daemon=True,
        )
        self._thread.start()
        self._started = True
        return self

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
        if self._thread is not None:
            self._thread.join(timeout=2.0)
        self._started = False
        self._server = None
        self._thread = None

    def __enter__(self) -> "MockDeviceService":
        return self.start()

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.stop()

    def _process_request(self, request: dict[str, Any]) -> dict[str, Any]:
        op = request.get("op", "submit")
        run_id = str(request.get("run_id", "default"))
        timeout_ms = float(request.get("timeout_ms", 30.0))
        raw_packets = request.get("packets")
        if not isinstance(raw_packets, list) or not raw_packets:
            raise ValueError("packets must be a non-empty list")

        packets: list[dict[str, Any]] = []
        for raw in raw_packets:
            data = bytes.fromhex(raw["data_hex"])
            header = decode_header(data)
            if header.magic != b"HTP1" or not header.crc_valid:
                raise ValueError(f"invalid packet bytes for seq {raw.get('packet_seq')}")
            if header.packet_seq != int(raw["packet_seq"]):
                raise ValueError("packet sequence in request does not match wire header")
            if header.channel != int(raw["channel"]):
                raise ValueError("packet channel in request does not match wire header")
            packets.append(
                {
                    "packet_seq": header.packet_seq,
                    "channel": header.channel,
                    "sent_at_ms": float(raw.get("sent_at_ms", 0.0)),
                    "data": data,
                }
            )

        with self._state.lock:
            self._state.batch_counter += 1
            batch_id = f"{run_id}:{self._state.batch_counter:06d}"

        if op == "retry":
            echoes = [
                self._make_echo(
                    run_id,
                    batch_id,
                    packets[0],
                    attempt=self._next_attempt(run_id, packets[0]["packet_seq"]),
                    rx_index=0,
                    delay_ms=0,
                )
            ]
            for index, echo in enumerate(echoes):
                echoes[index] = _replace_delivered(echo, index)
            return {"ok": True, "batch_id": batch_id, "echoes": [e.to_dict() for e in echoes]}

        echoes: list[Echo] = []
        rx_index = 0
        for packet in packets:
            attempt = self._next_attempt(run_id, packet["packet_seq"])
            if attempt == 1 and packet["packet_seq"] in self.fault_plan.drops:
                continue
            delay = self.fault_plan.delay_for(packet["packet_seq"]) if attempt == 1 else 0
            echo = self._make_echo(
                run_id,
                batch_id,
                packet,
                attempt=attempt,
                rx_index=rx_index,
                delay_ms=delay,
            )
            rx_index += 1
            echoes.append(echo)
            if attempt == 1 and packet["packet_seq"] in self.fault_plan.duplicates:
                duplicate = self._make_echo(
                    run_id,
                    batch_id,
                    packet,
                    attempt=attempt,
                    rx_index=rx_index,
                    delay_ms=delay + 1,
                )
                rx_index += 1
                echoes.append(duplicate)

        for left_seq, right_seq in self.fault_plan.reorder_pairs:
            left = next((i for i, echo in enumerate(echoes) if echo.received_seq == left_seq), None)
            right = next((i for i, echo in enumerate(echoes) if echo.received_seq == right_seq), None)
            if left is not None and right is not None:
                echoes[left], echoes[right] = echoes[right], echoes[left]

        echoes = [_replace_delivered(echo, index) for index, echo in enumerate(echoes)]
        return {"ok": True, "batch_id": batch_id, "echoes": [echo.to_dict() for echo in echoes]}

    def _next_attempt(self, run_id: str, packet_seq: int) -> int:
        with self._state.lock:
            key = (run_id, packet_seq)
            self._state.attempts[key] = self._state.attempts.get(key, 0) + 1
            return self._state.attempts[key]

    def _make_echo(
        self,
        run_id: str,
        batch_id: str,
        packet: dict[str, Any],
        *,
        attempt: int,
        rx_index: int,
        delay_ms: int,
    ) -> Echo:
        seq = int(packet["packet_seq"])
        sent_at = float(packet["sent_at_ms"])
        base_latency = 4.0 + float((seq * 7) % 11)
        processed_at = round(sent_at + base_latency + float(delay_ms), 3)
        return Echo(
            received_seq=seq,
            channel=int(packet["channel"]),
            service_rx_index=rx_index,
            delivered_index=rx_index,
            processed_at_ms=processed_at,
            attempt=attempt,
            batch_id=batch_id,
            wall_time_ns=time.time_ns(),
        )


def _replace_delivered(echo: Echo, delivered_index: int) -> Echo:
    return Echo(
        received_seq=echo.received_seq,
        channel=echo.channel,
        service_rx_index=echo.service_rx_index,
        delivered_index=delivered_index,
        processed_at_ms=echo.processed_at_ms,
        attempt=echo.attempt,
        batch_id=echo.batch_id,
        wall_time_ns=echo.wall_time_ns,
    )


class LoopbackClient:
    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 0,
        network_timeout_s: float = 5.0,
    ):
        self.host = host
        self.port = port
        self.network_timeout_s = network_timeout_s

    def send_batch(
        self,
        run_id: str,
        packets: Sequence[DispatchRecord],
        timeout_ms: float,
        *,
        retry: bool = False,
    ) -> tuple[Echo, ...]:
        if not packets:
            return ()
        request = {
            "op": "retry" if retry else "submit",
            "run_id": run_id,
            "timeout_ms": timeout_ms,
            "packets": [
                {
                    "packet_seq": packet.packet_seq,
                    "channel": packet.channel,
                    "sent_at_ms": packet.sent_at_ms,
                    "data_hex": packet.data.hex(),
                }
                for packet in packets
            ],
        }
        raw = json.dumps(request, ensure_ascii=False, separators=(",", ":")).encode("utf-8") + b"\n"
        try:
            with socket.create_connection(
                (self.host, self.port), timeout=self.network_timeout_s
            ) as sock:
                sock.sendall(raw)
                response_raw = bytearray()
                while not response_raw.endswith(b"\n"):
                    chunk = sock.recv(65536)
                    if not chunk:
                        break
                    response_raw.extend(chunk)
        except OSError as exc:
            raise HapticError(
                Issue(
                    ErrorCode.E_IO,
                    f"loopback dispatch failed: {exc}",
                    field="loopback",
                    details={"host": self.host, "port": self.port},
                )
            ) from exc
        try:
            response = json.loads(bytes(response_raw).decode("utf-8"))
        except json.JSONDecodeError as exc:
            raise HapticError(
                Issue(
                    ErrorCode.E_IO,
                    f"invalid loopback response: {exc}",
                    field="loopback",
                )
            ) from exc
        if not response.get("ok"):
            error = response.get("error", {})
            raise HapticError(
                Issue(
                    ErrorCode.E_IO,
                    str(error.get("message", "mock device rejected request")),
                    field="loopback",
                )
            )
        return tuple(
            Echo(
                received_seq=int(item["received_seq"]),
                channel=int(item["channel"]),
                service_rx_index=int(item["service_rx_index"]),
                delivered_index=int(item["delivered_index"]),
                processed_at_ms=float(item["processed_at_ms"]),
                attempt=int(item["attempt"]),
                batch_id=str(item["batch_id"]),
                wall_time_ns=int(item.get("wall_time_ns", 0)),
            )
            for item in response["echoes"]
        )

    def retry_packet(self, run_id: str, packet: DispatchRecord, timeout_ms: float) -> Echo:
        echoes = self.send_batch(run_id, [packet], timeout_ms, retry=True)
        if not echoes:
            raise HapticError(
                Issue(
                    ErrorCode.E_IO,
                    "mock device returned no echo for retry",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field="echo",
                )
            )
        return echoes[0]

    def submit(
        self,
        run_id: str,
        records: Iterable[DispatchRecord],
        timeout_ms: float,
    ) -> tuple[Echo, ...]:
        return self.send_batch(run_id, tuple(records), timeout_ms)

