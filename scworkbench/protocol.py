from __future__ import annotations

import hashlib
import json
import struct
import zlib
from dataclasses import dataclass, replace
from typing import Any

import numpy as np

from .errors import CorruptError, DecodeError, SessionMismatchError
from .models import InputEvent
from .profiles import CodecProfile, PROTOCOL_VERSION

MEDIA_MAGIC = b"SCWB"
WIRE_VERSION = 1
MEDIA_HEADER = struct.Struct("!4sBB16sIHHQIII")
MAX_FRAGMENTS = 65535


def _session_digest(session_id: str) -> bytes:
    return hashlib.sha256(session_id.encode("utf-8")).digest()[:16]


@dataclass(frozen=True)
class MediaPacket:
    session_digest: bytes
    frame_seq: int
    fragment_index: int
    fragment_count: int
    timestamp_ms: int
    payload: bytes
    frame_tag: int
    flags: int = 0

    @classmethod
    def create(
        cls,
        session_id: str,
        frame_seq: int,
        fragment_index: int,
        fragment_count: int,
        timestamp_ms: int,
        payload: bytes,
        frame_tag: int,
    ) -> "MediaPacket":
        return cls(
            session_digest=_session_digest(session_id),
            frame_seq=frame_seq,
            fragment_index=fragment_index,
            fragment_count=fragment_count,
            timestamp_ms=timestamp_ms,
            payload=payload,
            frame_tag=frame_tag,
        )

    def to_bytes(self) -> bytes:
        header = MEDIA_HEADER.pack(
            MEDIA_MAGIC,
            WIRE_VERSION,
            self.flags,
            self.session_digest,
            self.frame_seq,
            self.fragment_index,
            self.fragment_count,
            self.timestamp_ms,
            len(self.payload),
            zlib.crc32(self.payload) & 0xFFFFFFFF,
            self.frame_tag & 0xFFFFFFFF,
        )
        return header + self.payload

    @classmethod
    def from_bytes(cls, data: bytes) -> "MediaPacket":
        if len(data) < MEDIA_HEADER.size:
            raise CorruptError("媒体包头长度不足", actual=len(data))
        (
            magic,
            version,
            flags,
            session_digest,
            frame_seq,
            fragment_index,
            fragment_count,
            timestamp_ms,
            payload_len,
            payload_crc,
            frame_tag,
        ) = MEDIA_HEADER.unpack_from(data)
        if magic != MEDIA_MAGIC:
            raise CorruptError("媒体包 magic 损坏", actual=magic.hex())
        if version != WIRE_VERSION:
            raise CorruptError(
                "媒体包协议版本损坏", expected=WIRE_VERSION, actual=version
            )
        if fragment_count <= 0 or fragment_index >= fragment_count:
            raise CorruptError(
                "媒体包分片索引非法",
                fragment_index=fragment_index,
                fragment_count=fragment_count,
            )
        payload = data[MEDIA_HEADER.size :]
        if payload_len != len(payload):
            raise CorruptError(
                "媒体包 payload 长度不匹配",
                declared=payload_len,
                actual=len(payload),
            )
        if (zlib.crc32(payload) & 0xFFFFFFFF) != payload_crc:
            raise CorruptError(
                "媒体包 payload CRC 校验失败",
                frame_seq=frame_seq,
                fragment_index=fragment_index,
            )
        return cls(
            session_digest=session_digest,
            frame_seq=frame_seq,
            fragment_index=fragment_index,
            fragment_count=fragment_count,
            timestamp_ms=timestamp_ms,
            payload=payload,
            frame_tag=frame_tag,
            flags=flags,
        )

    def validate_session(self, session_id: str) -> None:
        expected = _session_digest(session_id)
        if self.session_digest != expected:
            raise SessionMismatchError(
                expected=expected.hex(),
                actual=self.session_digest.hex(),
                frame_seq=self.frame_seq,
            )

    def with_foreign_session(self) -> "MediaPacket":
        foreign = hashlib.sha256(b"foreign-session").digest()[:16]
        if foreign == self.session_digest:
            foreign = bytes(reversed(foreign))
        return replace(self, session_digest=foreign)

    def corrupted(self) -> "MediaPacket":
        if not self.payload:
            raise CorruptError("空 payload 无法注入损坏")
        payload = bytearray(self.payload)
        payload[len(payload) // 2] ^= 0xA5
        return replace(self, payload=bytes(payload))


@dataclass(frozen=True)
class EncodedFrame:
    packets: tuple[MediaPacket, ...]
    compressed: bytes
    frame_tag: int
    wire_sha256: str
    width: int
    height: int


def encode_frame_payload(
    image: np.ndarray,
    frame_seq: int,
    timestamp_ms: int,
    session_id: str,
    profile: CodecProfile,
) -> EncodedFrame:
    if image.dtype != np.uint8 or image.ndim != 3 or image.shape[2] != 4:
        raise DecodeError("待编码帧必须是 RGBA uint8 图像")
    raw = image.tobytes(order="C")
    compressed = zlib.compress(raw, profile.compression_level)
    frame_tag = zlib.crc32(compressed) & 0xFFFFFFFF
    fragment_bytes = max(1, profile.fragment_bytes)
    chunks = [
        compressed[offset : offset + fragment_bytes]
        for offset in range(0, len(compressed), fragment_bytes)
    ]
    if not chunks:
        chunks = [b""]
    if len(chunks) > MAX_FRAGMENTS:
        raise DecodeError(
            "单帧分片数超过协议上限",
            fragments=len(chunks),
            maximum=MAX_FRAGMENTS,
        )
    packets = tuple(
        MediaPacket.create(
            session_id=session_id,
            frame_seq=frame_seq,
            fragment_index=index,
            fragment_count=len(chunks),
            timestamp_ms=timestamp_ms,
            payload=chunk,
            frame_tag=frame_tag,
        )
        for index, chunk in enumerate(chunks)
    )
    return EncodedFrame(
        packets=packets,
        compressed=compressed,
        frame_tag=frame_tag,
        wire_sha256=hashlib.sha256(compressed).hexdigest(),
        width=image.shape[1],
        height=image.shape[0],
    )


@dataclass(frozen=True)
class InputEnvelope:
    session_id: str
    event: InputEvent

    def to_dict(self) -> dict[str, Any]:
        event = self.event.to_dict()
        digest = hashlib.sha256(
            json.dumps(
                event, ensure_ascii=False, sort_keys=True, separators=(",", ":")
            ).encode("utf-8")
        ).hexdigest()
        return {
            "protocol_version": PROTOCOL_VERSION,
            "session_id": self.session_id,
            "event": event,
            "event_sha256": digest,
        }

    def to_bytes(self) -> bytes:
        return json.dumps(
            self.to_dict(),
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")

    @classmethod
    def from_bytes(
        cls, data: bytes, expected_session_id: str | None = None
    ) -> "InputEnvelope":
        try:
            payload = json.loads(data.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise DecodeError("输入封装内容损坏", error=str(exc)) from exc
        if not isinstance(payload, dict):
            raise DecodeError("输入封装必须是 JSON 对象")
        if payload.get("protocol_version") != PROTOCOL_VERSION:
            raise DecodeError(
                "输入封装协议版本不匹配",
                expected=PROTOCOL_VERSION,
                actual=payload.get("protocol_version"),
            )
        event_data = payload.get("event")
        if not isinstance(event_data, dict):
            raise DecodeError("输入封装缺少 event")
        required = {"seq", "timestamp_ms", "type", "x", "y"}
        if not required.issubset(event_data):
            raise DecodeError("输入封装 event 字段缺失")
        event = InputEvent(
            seq=int(event_data["seq"]),
            timestamp_ms=int(event_data["timestamp_ms"]),
            kind=str(event_data["type"]),
            x=int(event_data["x"]),
            y=int(event_data["y"]),
            to_x=(
                None if event_data.get("to_x") is None else int(event_data["to_x"])
            ),
            to_y=(
                None if event_data.get("to_y") is None else int(event_data["to_y"])
            ),
            key=(
                None if event_data.get("key") is None else str(event_data["key"])
            ),
            delta=(
                None
                if event_data.get("delta") is None
                else int(event_data["delta"])
            ),
        )
        canonical = json.dumps(
            event.to_dict(),
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        expected_digest = hashlib.sha256(canonical).hexdigest()
        if payload.get("event_sha256") != expected_digest:
            raise CorruptError("输入封装内容哈希不匹配")
        session_id = str(payload.get("session_id", ""))
        if expected_session_id is not None and session_id != expected_session_id:
            raise SessionMismatchError(
                expected=expected_session_id, actual=session_id
            )
        return cls(session_id=session_id, event=event)
