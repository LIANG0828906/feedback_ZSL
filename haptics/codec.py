from __future__ import annotations

import struct
import zlib
from dataclasses import dataclass
from concurrent.futures import ThreadPoolExecutor
from typing import Any

from .errors import ErrorCode, HapticError, Issue
from .models import (
    DeviceCapabilities,
    EncodedPacket,
    PacketState,
    ScheduledPacket,
    ScheduledStream,
)
from .limits import ResourceLimits

MAGIC = b"HTP1"
FORMAT_VERSION = 1
HEADER_STRUCT = struct.Struct(">4sBHIHHII")
SLOT_STRUCT = struct.Struct(">HHBBBHHI")
CRC_STRUCT = struct.Struct(">I")
TYPE_TO_CODE = {
    "hit": 1,
    "shoot": 2,
    "explosion": 3,
    "footstep": 4,
    "ui": 5,
}
CODE_TO_TYPE = {code: name for name, code in TYPE_TO_CODE.items()}


@dataclass(frozen=True)
class DecodedHeader:
    magic: bytes
    format_version: int
    capability_version: int
    packet_seq: int
    channel: int
    timing_mark_ms: int
    event_count: int
    payload_length: int
    crc32: int
    crc_valid: bool
    total_length: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "magic": self.magic.decode("ascii", errors="replace"),
            "format_version": self.format_version,
            "capability_version": self.capability_version,
            "packet_seq": self.packet_seq,
            "channel": self.channel,
            "timing_mark_ms": self.timing_mark_ms,
            "event_count": self.event_count,
            "payload_length": self.payload_length,
            "crc32": self.crc32,
            "crc_valid": self.crc_valid,
            "total_length": self.total_length,
        }


def _u8(value: float) -> int:
    return max(0, min(255, int(round(value * 255.0))))


def _u16(value: float) -> int:
    return max(0, min(65535, int(round(value * 65535.0))))


def _validate_packet(packet: ScheduledPacket, capabilities: DeviceCapabilities) -> None:
    capability = capabilities.channel(packet.channel)
    if packet.event_count > capability.max_events_per_packet:
        raise HapticError(
            Issue(
                ErrorCode.E_PACK,
                "packet event count exceeds channel capability",
                packet_seq=packet.packet_seq,
                channel=packet.channel,
                field="event_count",
                details={
                    "value": packet.event_count,
                    "maximum": capability.max_events_per_packet,
                },
            )
        )
    if packet.event_count < 1:
        raise HapticError(
            Issue(
                ErrorCode.E_PACK,
                "packet must contain at least one event",
                packet_seq=packet.packet_seq,
                channel=packet.channel,
                field="event_count",
                details={"value": packet.event_count},
            )
        )
    window_min, window_max = capability.packet_window_ms
    if not (window_min - 1e-9 <= packet.timing_mark_ms <= window_max + 1e-9):
        raise HapticError(
            Issue(
                ErrorCode.E_PACK,
                "packet timing mark is outside the capability window",
                packet_seq=packet.packet_seq,
                channel=packet.channel,
                field="timing_mark_ms",
                details={
                    "value": packet.timing_mark_ms,
                    "window_ms": [window_min, window_max],
                },
            )
        )
    for index, slot in enumerate(packet.slots):
        offset = slot.time_ms - packet.start_ms + window_min
        if offset < window_min - 1e-9 or offset > window_max + 1e-9:
            raise HapticError(
                Issue(
                    ErrorCode.E_PACK,
                    "event timing offset is outside the capability window",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field=f"slots[{index}].offset_ms",
                    details={
                        "value": round(offset, 6),
                        "window_ms": [window_min, window_max],
                    },
                )
            )
        if offset > 65535:
            raise HapticError(
                Issue(
                    ErrorCode.E_PACK,
                    "event timing offset exceeds the encoded field width",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field=f"slots[{index}].offset_ms",
                    details={"value": offset, "maximum": 65535},
                )
            )
        if slot.event_type not in TYPE_TO_CODE:
            raise HapticError(
                Issue(
                    ErrorCode.E_PACK,
                    "event type has no stable wire code",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field=f"slots[{index}].type",
                    details={"value": slot.event_type},
                )
            )
        if len(slot.envelope) > 65535 or len(slot.source_event_ids) > 65535:
            raise HapticError(
                Issue(
                    ErrorCode.E_PACK,
                    "packet slot exceeds encoded field width",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field=f"slots[{index}]",
                )
            )


def encode_packet(
    packet: ScheduledPacket,
    capabilities: DeviceCapabilities,
) -> EncodedPacket:
    """Encode one scheduled packet into canonical bytes.

    Calling this function twice with the same arguments always returns exactly
    the same bytes.  No timestamps, thread identifiers, or dict iteration
    order enter the wire format.
    """

    _validate_packet(packet, capabilities)
    payload = bytearray()
    window_min = capabilities.channel(packet.channel).packet_window_ms[0]
    for slot in packet.slots:
        offset_ms = int(
            round(slot.time_ms - packet.start_ms + window_min)
        )
        flags = 1 if slot.no_interrupt else 0
        envelope = bytes(_u8(value) for value in slot.envelope)
        payload.extend(
            SLOT_STRUCT.pack(
                slot.slot_index,
                offset_ms,
                TYPE_TO_CODE[slot.event_type],
                slot.priority,
                flags,
                _u16(slot.intensity),
                len(slot.source_event_ids),
                len(envelope),
            )
        )
        payload.extend(envelope)

    header = HEADER_STRUCT.pack(
        MAGIC,
        FORMAT_VERSION,
        capabilities.capability_version,
        packet.packet_seq,
        packet.channel,
        int(round(packet.timing_mark_ms)),
        packet.event_count,
        len(payload),
    )
    body = header + bytes(payload)
    crc = zlib.crc32(body) & 0xFFFFFFFF
    data = body + CRC_STRUCT.pack(crc)
    return EncodedPacket(
        packet_seq=packet.packet_seq,
        channel=packet.channel,
        data=data,
        timing_mark_ms=packet.timing_mark_ms,
        event_count=packet.event_count,
        local_index=packet.local_index,
        state=PacketState.PENDING,
        reused=packet.reused,
        invalidated=packet.invalidated,
    )


def encode_stream(
    scheduled: ScheduledStream,
    capabilities: DeviceCapabilities,
) -> tuple[EncodedPacket, ...]:
    return tuple(encode_packet(packet, capabilities) for packet in scheduled.packets)


def encode_stream_parallel(
    scheduled: ScheduledStream,
    capabilities: DeviceCapabilities,
    limits: ResourceLimits = ResourceLimits(),
) -> tuple[EncodedPacket, ...]:
    limits.validate()
    packets = list(scheduled.packets)
    chunk_size = limits.max_packets_per_task
    chunks = [packets[index : index + chunk_size] for index in range(0, len(packets), chunk_size)]

    def encode_chunk(chunk: list[ScheduledPacket]) -> list[tuple[int, EncodedPacket]]:
        return [(packet.packet_seq, encode_packet(packet, capabilities)) for packet in chunk]

    indexed: list[tuple[int, EncodedPacket]] = []
    with ThreadPoolExecutor(
        max_workers=min(limits.max_parallelism, max(1, len(chunks))),
        thread_name_prefix="haptic-encode",
    ) as pool:
        for part in pool.map(encode_chunk, chunks):
            indexed.extend(part)
    indexed.sort(key=lambda item: item[0])
    return tuple(packet for _, packet in indexed)


def decode_header(data: bytes) -> DecodedHeader:
    if len(data) < HEADER_STRUCT.size + CRC_STRUCT.size:
        raise HapticError(
            Issue(
                ErrorCode.E_PACK,
                "device command packet is shorter than the minimum frame",
                field="packet",
            )
        )
    (
        magic,
        format_version,
        capability_version,
        packet_seq,
        channel,
        timing_mark_ms,
        event_count,
        payload_length,
    ) = HEADER_STRUCT.unpack_from(data, 0)
    total_length = HEADER_STRUCT.size + payload_length + CRC_STRUCT.size
    if total_length != len(data):
        raise HapticError(
            Issue(
                ErrorCode.E_PACK,
                "device command packet length does not match header",
                packet_seq=packet_seq,
                channel=channel,
                field="payload_length",
                details={"expected": total_length, "actual": len(data)},
            )
        )
    expected_crc = CRC_STRUCT.unpack_from(data, len(data) - CRC_STRUCT.size)[0]
    actual_crc = zlib.crc32(data[: -CRC_STRUCT.size]) & 0xFFFFFFFF
    return DecodedHeader(
        magic=magic,
        format_version=format_version,
        capability_version=capability_version,
        packet_seq=packet_seq,
        channel=channel,
        timing_mark_ms=timing_mark_ms,
        event_count=event_count,
        payload_length=payload_length,
        crc32=expected_crc,
        crc_valid=expected_crc == actual_crc,
        total_length=total_length,
    )

