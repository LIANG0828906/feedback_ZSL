from __future__ import annotations

import json
import socket
import socketserver
import threading
from dataclasses import dataclass, field
from typing import Any

from .errors import CorruptError, DecodeError
from .protocol import InputEnvelope, MediaPacket

MEDIA_REQUEST = 0x01
INPUT_REQUEST = 0x02
PING_REQUEST = 0x10
MEDIA_RESPONSE = 0x81
INPUT_RESPONSE = 0x82
PING_RESPONSE = 0x90
ERROR_RESPONSE = 0xE1


@dataclass
class RelayStats:
    media_received: int = 0
    input_received: int = 0
    pings: int = 0
    invalid_requests: int = 0
    bytes_received: int = 0
    sessions: set[str] = field(default_factory=set)

    def snapshot(self) -> dict[str, Any]:
        return {
            "media_received": self.media_received,
            "input_received": self.input_received,
            "pings": self.pings,
            "invalid_requests": self.invalid_requests,
            "bytes_received": self.bytes_received,
            "sessions": sorted(self.sessions),
        }


class _RelayUdpServer(socketserver.ThreadingUDPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, server_address: tuple[str, int]) -> None:
        self.stats = RelayStats()
        self.stats_lock = threading.Lock()
        super().__init__(server_address, _RelayHandler)


class _RelayHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        data, sock = self.request
        server: _RelayUdpServer = self.server  # type: ignore[assignment]
        if not data:
            return
        with server.stats_lock:
            server.stats.bytes_received += len(data)
        kind = data[0]
        payload = data[1:]
        if kind == PING_REQUEST:
            with server.stats_lock:
                server.stats.pings += 1
            sock.sendto(bytes([PING_RESPONSE]) + b"pong", self.client_address)
            return
        if kind == MEDIA_REQUEST:
            try:
                MediaPacket.from_bytes(payload)
            except CorruptError as exc:
                with server.stats_lock:
                    server.stats.invalid_requests += 1
                sock.sendto(
                    bytes([ERROR_RESPONSE])
                    + json.dumps(
                        {"code": exc.code, "message": exc.message},
                        ensure_ascii=False,
                    ).encode("utf-8"),
                    self.client_address,
                )
                return
            with server.stats_lock:
                server.stats.media_received += 1
            sock.sendto(bytes([MEDIA_RESPONSE]) + payload, self.client_address)
            return
        if kind == INPUT_REQUEST:
            try:
                envelope = InputEnvelope.from_bytes(payload)
            except (DecodeError, CorruptError) as exc:
                with server.stats_lock:
                    server.stats.invalid_requests += 1
                sock.sendto(
                    bytes([ERROR_RESPONSE])
                    + json.dumps(
                        {"code": exc.code, "message": exc.message},
                        ensure_ascii=False,
                    ).encode("utf-8"),
                    self.client_address,
                )
                return
            with server.stats_lock:
                server.stats.input_received += 1
                server.stats.sessions.add(envelope.session_id)
            sock.sendto(bytes([INPUT_RESPONSE]) + payload, self.client_address)
            return
        with server.stats_lock:
            server.stats.invalid_requests += 1
        sock.sendto(
            bytes([ERROR_RESPONSE])
            + b'{"code":"E_DECODE","message":"unknown relay request"}',
            self.client_address,
        )


class LoopbackRelayServer:
    def __init__(self, host: str = "127.0.0.1", port: int = 0) -> None:
        if host != "127.0.0.1":
            raise ValueError("relay must bind to local loopback 127.0.0.1")
        self._server = _RelayUdpServer((host, port))
        self._thread: threading.Thread | None = None
        self.host, self.port = self._server.server_address[:2]

    def start(self) -> "LoopbackRelayServer":
        if self._thread is not None:
            return self
        self._thread = threading.Thread(
            target=self._server.serve_forever,
            name="cast-loopback-relay",
            daemon=True,
        )
        self._thread.start()
        return self

    def stats(self) -> dict[str, Any]:
        with self._server.stats_lock:
            return self._server.stats.snapshot()

    def stop(self) -> None:
        if self._thread is None:
            return
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=2.0)
        self._thread = None

    def __enter__(self) -> "LoopbackRelayServer":
        return self.start()

    def __exit__(self, *args: Any) -> None:
        self.stop()


class LoopbackUdpClient:
    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 0,
        timeout: float = 2.0,
    ) -> None:
        self.address = (host, int(port))
        self.timeout = timeout
        self._socket = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self._socket.settimeout(timeout)
        self._lock = threading.Lock()
        self._closed = False

    def _request(self, request: bytes, expected_response: int) -> bytes:
        with self._lock:
            if self._closed:
                raise DecodeError("local loopback transport is closed")
            try:
                self._socket.sendto(request, self.address)
                response, _ = self._socket.recvfrom(65535)
            except OSError as exc:
                raise DecodeError(
                    "local loopback transport failed", error=str(exc)
                ) from exc
        if not response:
            raise DecodeError("empty loopback response")
        if response[0] == ERROR_RESPONSE:
            try:
                error = json.loads(response[1:].decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                error = {"code": "E_DECODE", "message": "invalid error response"}
            if error.get("code") == "E_CORRUPT":
                raise CorruptError(error.get("message", "relay corruption"))
            raise DecodeError(error.get("message", "relay rejected request"))
        if response[0] != expected_response:
            raise DecodeError(
                "unexpected loopback response type",
                expected=expected_response,
                actual=response[0],
            )
        return response[1:]

    def send_media(self, packet: bytes) -> bytes:
        return self._request(bytes([MEDIA_REQUEST]) + packet, MEDIA_RESPONSE)

    def send_input(self, envelope: bytes) -> bytes:
        return self._request(bytes([INPUT_REQUEST]) + envelope, INPUT_RESPONSE)

    def ping(self) -> bool:
        return self._request(bytes([PING_REQUEST]), PING_RESPONSE) == b"pong"

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
            self._socket.close()
