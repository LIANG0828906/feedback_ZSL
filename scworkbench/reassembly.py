from __future__ import annotations

import hashlib
import zlib
from dataclasses import dataclass, field

import numpy as np

from .errors import CorruptError
from .protocol import MediaPacket


@dataclass
class ReassembledFrame:
    frame_seq: int
    timestamp_ms: int
    status: str
    image: np.ndarray | None
    compressed: bytes
    image_sha256: str
    wire_sha256: str
    codes: list[str] = field(default_factory=list)
    missing_fragments: list[int] = field(default_factory=list)
    received_fragments: int = 0
    duplicate_fragments: int = 0
    order_events: int = 0
    message: str = ""


class FrameReassembler:
    def __init__(
        self,
        session_id: str,
        width: int,
        height: int,
        max_fragment_count: int = 65535,
    ) -> None:
        self.session_id = session_id
        self.width = width
        self.height = height
        self.max_fragment_count = max_fragment_count
        self._parts: dict[int, MediaPacket] = {}
        self._fragment_count: int | None = None
        self._frame_tag: int | None = None
        self._frame_seq: int | None = None
        self._timestamp_ms: int | None = None
        self._next_expected = 0
        self._accepted = 0
        self._duplicates = 0
        self.order_events = 0
        self.corrupt_events = 0

    def reset(self) -> None:
        self.__init__(
            self.session_id,
            self.width,
            self.height,
            self.max_fragment_count,
        )

    def accept_bytes(self, data: bytes) -> MediaPacket | None:
        try:
            packet = MediaPacket.from_bytes(data)
        except CorruptError:
            self.corrupt_events += 1
            raise
        packet.validate_session(self.session_id)
        return self.accept(packet)

    def accept(self, packet: MediaPacket) -> MediaPacket | None:
        packet.validate_session(self.session_id)
        if packet.fragment_count > self.max_fragment_count:
            raise CorruptError(
                "分片数量超过接收端缓冲上限",
                fragment_count=packet.fragment_count,
                maximum=self.max_fragment_count,
            )
        if self._frame_seq is None:
            self._frame_seq = packet.frame_seq
            self._timestamp_ms = packet.timestamp_ms
            self._fragment_count = packet.fragment_count
            self._frame_tag = packet.frame_tag
        elif (
            packet.frame_seq != self._frame_seq
            or packet.timestamp_ms != self._timestamp_ms
            or packet.fragment_count != self._fragment_count
            or packet.frame_tag != self._frame_tag
        ):
            raise CorruptError(
                "同帧分片元数据不一致",
                expected_frame=self._frame_seq,
                actual_frame=packet.frame_seq,
            )
        if packet.fragment_index in self._parts:
            self._duplicates += 1
            return None
        if packet.fragment_index != self._next_expected:
            self.order_events += 1
        self._next_expected = max(self._next_expected, packet.fragment_index + 1)
        self._parts[packet.fragment_index] = packet
        self._accepted += 1
        return packet

    @property
    def received_count(self) -> int:
        return self._accepted

    @property
    def duplicate_count(self) -> int:
        return self._duplicates

    @property
    def fragment_count(self) -> int:
        return self._fragment_count or 0

    def missing(self, total: int | None = None) -> list[int]:
        expected = self._fragment_count if total is None else total
        if expected is None:
            return []
        return [index for index in range(expected) if index not in self._parts]

    def finalize(self) -> ReassembledFrame:
        frame_seq = self._frame_seq or 0
        timestamp_ms = self._timestamp_ms or 0
        total = self._fragment_count or 0
        missing = self.missing()
        codes: list[str] = []
        if self.order_events:
            codes.append("E_ORDER")
        if missing:
            codes.append("E_LOSS")
            return ReassembledFrame(
                frame_seq=frame_seq,
                timestamp_ms=timestamp_ms,
                status="missing",
                image=None,
                compressed=b"",
                image_sha256="",
                wire_sha256="",
                codes=codes,
                missing_fragments=missing,
                received_fragments=self._accepted,
                duplicate_fragments=self._duplicates,
                order_events=self.order_events,
                message="重传上限耗尽，帧置为缺失",
            )
        if self._frame_tag is None or total <= 0:
            codes.append("E_CORRUPT")
            return ReassembledFrame(
                frame_seq=frame_seq,
                timestamp_ms=timestamp_ms,
                status="failed",
                image=None,
                compressed=b"",
                image_sha256="",
                wire_sha256="",
                codes=codes,
                missing_fragments=self.missing(total),
                received_fragments=self._accepted,
                duplicate_fragments=self._duplicates,
                order_events=self.order_events,
                message="帧元数据缺失",
            )
        compressed = b"".join(
            self._parts[index].payload for index in range(total)
        )
        if (zlib.crc32(compressed) & 0xFFFFFFFF) != self._frame_tag:
            codes.append("E_CORRUPT")
            return ReassembledFrame(
                frame_seq=frame_seq,
                timestamp_ms=timestamp_ms,
                status="failed",
                image=None,
                compressed=compressed,
                image_sha256="",
                wire_sha256=hashlib.sha256(compressed).hexdigest(),
                codes=codes,
                missing_fragments=[],
                received_fragments=self._accepted,
                duplicate_fragments=self._duplicates,
                order_events=self.order_events,
                message="重组帧内容校验失败",
            )
        try:
            raw = zlib.decompress(compressed)
        except zlib.error as exc:
            codes.append("E_CORRUPT")
            return ReassembledFrame(
                frame_seq=frame_seq,
                timestamp_ms=timestamp_ms,
                status="failed",
                image=None,
                compressed=compressed,
                image_sha256="",
                wire_sha256=hashlib.sha256(compressed).hexdigest(),
                codes=codes,
                received_fragments=self._accepted,
                duplicate_fragments=self._duplicates,
                order_events=self.order_events,
                message=f"帧解压失败: {exc}",
            )
        expected_bytes = self.width * self.height * 4
        if len(raw) != expected_bytes:
            codes.append("E_CORRUPT")
            return ReassembledFrame(
                frame_seq=frame_seq,
                timestamp_ms=timestamp_ms,
                status="failed",
                image=None,
                compressed=compressed,
                image_sha256="",
                wire_sha256=hashlib.sha256(compressed).hexdigest(),
                codes=codes,
                received_fragments=self._accepted,
                duplicate_fragments=self._duplicates,
                order_events=self.order_events,
                message="帧尺寸与协商分辨率不一致",
            )
        image = np.frombuffer(raw, dtype=np.uint8).reshape(
            self.height, self.width, 4
        )
        return ReassembledFrame(
            frame_seq=frame_seq,
            timestamp_ms=timestamp_ms,
            status="complete",
            image=image,
            compressed=compressed,
            image_sha256=hashlib.sha256(raw).hexdigest(),
            wire_sha256=hashlib.sha256(compressed).hexdigest(),
            codes=codes,
            missing_fragments=[],
            received_fragments=self._accepted,
            duplicate_fragments=self._duplicates,
            order_events=self.order_events,
        )
