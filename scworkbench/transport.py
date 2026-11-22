from __future__ import annotations

from typing import Protocol


class LoopbackTransport(Protocol):
    def send_media(self, packet: bytes) -> bytes:
        ...

    def send_input(self, envelope: bytes) -> bytes:
        ...

    def close(self) -> None:
        ...


class DirectTransport:
    """In-process loopback used by deterministic tests."""

    def __init__(self) -> None:
        self.media_packets = 0
        self.input_envelopes = 0

    def send_media(self, packet: bytes) -> bytes:
        self.media_packets += 1
        return packet

    def send_input(self, envelope: bytes) -> bytes:
        self.input_envelopes += 1
        return envelope

    def close(self) -> None:
        return None
