"""Three local TCP egress proxies with HTTP/1.1 chunked relay support.

Each proxy listens on 127.0.0.1 with a deterministic port from configuration,
emulates one WAN path through :class:`~edge_sync.link.LinkEmulator`, and
forwards requests to the shared local target HTTP service.  The proxy is
framing aware so it can:

* pace full requests/responses (including chunked upload bodies);
* inject a per-connection ``X-Link-Stats`` header into the response;
* enforce the configured per-egress active-connection cap.
"""

from __future__ import annotations

import json
import socket
import socketserver
import threading
import time

from . import ERR_EGRESS_CAP
from .config import EgressConfig
from .link import EgressLinkState, LinkEmulator, downstream_delay_ms


class SocketBuffer:
    """Minimal buffered socket reader with line/exact-length reads."""

    def __init__(self, sock: socket.socket):
        self.sock = sock
        self.buffer = bytearray()

    def _fill(self) -> None:
        chunk = self.sock.recv(65536)
        if not chunk:
            raise ConnectionError("peer closed connection")
        self.buffer.extend(chunk)

    def read_until(self, marker: bytes) -> bytes:
        while marker not in self.buffer:
            self._fill()
        index = self.buffer.index(marker) + len(marker)
        result = bytes(self.buffer[:index])
        del self.buffer[:index]
        return result

    def read_exact(self, length: int) -> bytes:
        while len(self.buffer) < length:
            self._fill()
        result = bytes(self.buffer[:length])
        del self.buffer[:length]
        return result

    def drain(self) -> bytes:
        remaining = bytes(self.buffer)
        self.buffer.clear()
        return remaining


def _forward(emulator: LinkEmulator, upstream: socket.socket, chunks: list[bytes]) -> None:
    for chunk in chunks:
        if not chunk:
            continue
        for segment in emulator.process(chunk):
            upstream.sendall(segment)


def relay_request(client: socket.socket, upstream: socket.socket, emulator: LinkEmulator) -> dict:
    """Relay one HTTP/1.1 request (chunked or length-delimited) upstream."""
    reader = SocketBuffer(client)
    head = reader.read_until(b"\r\n\r\n")
    headers_text = head.decode("iso-8859-1")
    header_lines = headers_text.split("\r\n")
    lower = {line.split(":", 1)[0].lower(): line for line in header_lines[1:] if ":" in line}
    _forward(emulator, upstream, [head])
    transfer_encoding = "transfer-encoding" in lower
    content_length = None
    if "content-length" in lower:
        try:
            content_length = int(lower["content-length"].split(":", 1)[1].strip())
        except ValueError:
            content_length = 0
    if transfer_encoding:
        while True:
            size_line = reader.read_until(b"\r\n")
            _forward(emulator, upstream, [size_line])
            chunk_size = int(size_line.strip().split(b";", 1)[0], 16)
            if chunk_size == 0:
                # Optional trailers terminate with a blank line.
                trailers = reader.read_until(b"\r\n")
                _forward(emulator, upstream, [trailers])
                break
            _forward(emulator, upstream, [reader.read_exact(chunk_size), reader.read_exact(2)])
    elif content_length:
        _forward(emulator, upstream, [reader.read_exact(content_length)])
    leftover = reader.drain()
    if leftover:
        _forward(emulator, upstream, [leftover])
    return {"method": header_lines[0]}


def relay_response(upstream: socket.socket, client: socket.socket, emulator: LinkEmulator) -> None:
    """Relay the target response and add the per-connection link header."""
    reader = SocketBuffer(upstream)
    status_block = reader.read_until(b"\r\n\r\n")
    time.sleep(downstream_delay_ms(emulator.state) / 1000.0)
    link_header = emulator.stats_header()
    injected = "".join(f"{key}: {value}\r\n" for key, value in link_header.items()).encode("ascii")
    client.sendall(status_block[:-2] + injected + b"\r\n")
    headers = status_block.decode("iso-8859-1").lower()
    if "transfer-encoding: chunked" in headers:
        while True:
            size_line = reader.read_until(b"\r\n")
            chunk_size = int(size_line.strip().split(b";", 1)[0], 16)
            client.sendall(size_line)
            if chunk_size == 0:
                client.sendall(reader.read_until(b"\r\n"))
                break
            payload = reader.read_exact(chunk_size + 2)
            emulator.state.take_bandwidth(len(payload))
            client.sendall(payload)
    elif "content-length:" in headers:
        length = 0
        for line in status_block.decode("iso-8859-1").split("\r\n"):
            if line.lower().startswith("content-length:"):
                length = int(line.split(":", 1)[1].strip())
        if length:
            payload = reader.read_exact(length)
            emulator.state.take_bandwidth(len(payload))
            client.sendall(payload)
    else:
        while True:
            try:
                chunk = upstream.recv(65536)
            except OSError:
                break
            if not chunk:
                break
            client.sendall(chunk)


class _ThreadingServer(socketserver.ThreadingTCPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128


class EgressProxy:
    def __init__(self, config: EgressConfig, index: int, seed: int, injections, target: tuple[str, int]):
        self.config = config
        self.target = target
        self.state = EgressLinkState(
            egress_id=config.egress_id,
            egress_index=index,
            seed=seed,
            base_rtt_ms=config.rtt_ms,
            base_loss_pct=config.loss_pct,
            bandwidth_mbps=config.bandwidth_mbps,
            injections=tuple(injections),
        )
        self._server: _ThreadingServer | None = None
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        state = self.state
        target = self.target
        cap = self.config.concurrency

        class Handler(socketserver.BaseRequestHandler):
            def handle(self) -> None:
                with state.lock:
                    if state.active_connections >= cap:
                        body = json.dumps({"error": ERR_EGRESS_CAP, "egress": state.egress_id}).encode()
                        self.request.sendall(
                            b"HTTP/1.1 503 Egress Cap\r\n"
                            b"Content-Type: application/json\r\n"
                            b"Connection: close\r\n"
                            b"Content-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body
                        )
                        return
                    state.active_connections += 1
                emulator = LinkEmulator(state)
                try:
                    self.request.settimeout(90)
                    with socket.create_connection(target, timeout=30) as upstream:
                        upstream.settimeout(90)
                        relay_request(self.request, upstream, emulator)
                        try:
                            upstream.shutdown(socket.SHUT_WR)
                        except OSError:
                            pass
                        relay_response(upstream, self.request, emulator)
                except (OSError, ConnectionError, ValueError):
                    pass
                finally:
                    with state.lock:
                        state.active_connections -= 1

        self._server = _ThreadingServer((self.config.listen_host, self.config.listen_port), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, name=f"proxy-{self.config.egress_id}", daemon=True)
        self._thread.start()

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None

    def address(self) -> tuple[str, int]:
        return self.config.listen_host, self.config.listen_port


class EgressProxyGroup:
    def __init__(self, configs, seed: int, injections, target: tuple[str, int]):
        self.proxies = [
            EgressProxy(config, index, seed, injections, target)
            for index, config in enumerate(configs)
        ]

    def start_all(self) -> None:
        for proxy in self.proxies:
            proxy.start()

    def stop_all(self) -> None:
        for proxy in self.proxies:
            proxy.stop()

    def states(self) -> dict[str, EgressLinkState]:
        return {proxy.config.egress_id: proxy.state for proxy in self.proxies}
