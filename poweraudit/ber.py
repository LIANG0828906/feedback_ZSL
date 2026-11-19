"""Small SNMP v2c BER codec and message helpers.

Only the subset needed for read-only GET/GETNEXT/GETBULK simulations and
robust local polling is implemented. No device state is ever written by the
service.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable, Sequence


class BerError(ValueError):
    pass


TAG_INTEGER = 0x02
TAG_OCTET_STRING = 0x04
TAG_NULL = 0x05
TAG_OID = 0x06
TAG_SEQUENCE = 0x30
TAG_GET_REQUEST = 0xA0
TAG_GET_NEXT_REQUEST = 0xA1
TAG_GET_RESPONSE = 0xA2
TAG_SET_REQUEST = 0xA3
TAG_GET_BULK_REQUEST = 0xA5
TAG_NO_SUCH_OBJECT = 0x80
TAG_NO_SUCH_INSTANCE = 0x81
TAG_END_OF_MIB_VIEW = 0x82


def encode_length(length: int) -> bytes:
    if length < 0:
        raise BerError("negative length")
    if length < 0x80:
        return bytes([length])
    encoded = length.to_bytes((length.bit_length() + 7) // 8, "big")
    return bytes([0x80 | len(encoded)]) + encoded


def tlv(tag: int, value: bytes) -> bytes:
    return bytes([tag]) + encode_length(len(value)) + value


def encode_integer(value: int) -> bytes:
    if value == 0:
        body = b"\x00"
    elif value > 0:
        body = value.to_bytes((value.bit_length() + 7) // 8, "big")
        if body[0] & 0x80:
            body = b"\x00" + body
    else:
        size = max(1, (value.bit_length() + 8) // 8)
        body = value.to_bytes(size, "big", signed=True)
        while len(body) > 1 and body[0] == 0xFF and body[1] & 0x80:
            body = body[1:]
    return tlv(TAG_INTEGER, body)


def encode_octet_string(value: str | bytes) -> bytes:
    body = value.encode("utf-8") if isinstance(value, str) else value
    return tlv(TAG_OCTET_STRING, body)


def encode_null() -> bytes:
    return tlv(TAG_NULL, b"")


def _encode_subidentifier(value: int) -> bytes:
    if value < 0:
        raise BerError("negative OID subidentifier")
    parts = [value & 0x7F]
    value >>= 7
    while value:
        parts.append(0x80 | (value & 0x7F))
        value >>= 7
    return bytes(reversed(parts))


def oid_to_tuple(oid: str | Sequence[int]) -> tuple[int, ...]:
    if isinstance(oid, str):
        text = oid.strip()
        if text.startswith("."):
            text = text[1:]
        if not text:
            raise BerError("empty OID")
        try:
            parts = tuple(int(part) for part in text.split("."))
        except ValueError as exc:
            raise BerError(f"invalid OID: {oid}") from exc
    else:
        parts = tuple(int(part) for part in oid)
    if len(parts) < 2 or parts[0] not in (0, 1, 2):
        raise BerError(f"invalid OID: {oid}")
    if parts[0] < 2 and parts[1] > 39:
        raise BerError(f"invalid OID: {oid}")
    return parts


def tuple_to_oid(oid: Sequence[int]) -> str:
    return "." + ".".join(str(part) for part in oid)


def encode_oid(oid: str | Sequence[int]) -> bytes:
    parts = oid_to_tuple(oid)
    body = bytearray(_encode_subidentifier(parts[0] * 40 + parts[1]))
    for part in parts[2:]:
        body.extend(_encode_subidentifier(part))
    return tlv(TAG_OID, bytes(body))


def _decode_subidentifier(data: bytes, offset: int) -> tuple[int, int]:
    value = 0
    while True:
        if offset >= len(data):
            raise BerError("truncated OID subidentifier")
        byte = data[offset]
        offset += 1
        value = (value << 7) | (byte & 0x7F)
        if not byte & 0x80:
            return value, offset


def decode_oid(data: bytes, offset: int = 0) -> tuple[tuple[int, ...], int]:
    first, offset = _decode_subidentifier(data, offset)
    if first < 40:
        parts = [0, first]
    elif first < 80:
        parts = [1, first - 40]
    else:
        parts = [2, first - 80]
    while offset < len(data):
        value, offset = _decode_subidentifier(data, offset)
        parts.append(value)
    return tuple(parts), offset


def read_tlv(data: bytes, offset: int = 0) -> tuple[int, bytes, int]:
    if offset >= len(data):
        raise BerError("missing BER tag")
    tag = data[offset]
    offset += 1
    if offset >= len(data):
        raise BerError("missing BER length")
    first = data[offset]
    offset += 1
    if first < 0x80:
        length = first
    else:
        count = first & 0x7F
        if count == 0:
            raise BerError("indefinite BER length is not supported")
        if offset + count > len(data):
            raise BerError("truncated BER length")
        length = int.from_bytes(data[offset : offset + count], "big")
        offset += count
    end = offset + length
    if end > len(data):
        raise BerError("truncated BER value")
    return tag, data[offset:end], end


def decode_integer(data: bytes) -> int:
    if not data:
        raise BerError("empty INTEGER")
    return int.from_bytes(data, "big", signed=True)


@dataclass(frozen=True)
class Varbind:
    oid: tuple[int, ...]
    tag: int
    value: Any

    @property
    def oid_text(self) -> str:
        return tuple_to_oid(self.oid)


@dataclass(frozen=True)
class SnmpMessage:
    version: int
    community: str
    pdu_tag: int
    request_id: int
    error_status: int
    error_index: int
    varbinds: tuple[Varbind, ...]


def decode_value(tag: int, body: bytes) -> Any:
    if tag in (TAG_NULL, TAG_NO_SUCH_OBJECT, TAG_NO_SUCH_INSTANCE, TAG_END_OF_MIB_VIEW):
        return None
    if tag == TAG_INTEGER:
        return decode_integer(body)
    if tag == TAG_OCTET_STRING:
        try:
            return body.decode("utf-8")
        except UnicodeDecodeError:
            return body.hex()
    if tag == TAG_OID:
        return decode_oid(body)[0]
    # Gauge32/Counter32/TimeTicks and other integer-like values.
    if tag in (0x41, 0x42, 0x43, 0x46):
        return int.from_bytes(body, "big")
    if tag == 0x40 and len(body) == 4:
        return ".".join(str(part) for part in body)
    return body.hex()


def encode_value(tag: int, value: Any) -> bytes:
    if tag in (TAG_NULL, TAG_NO_SUCH_OBJECT, TAG_NO_SUCH_INSTANCE, TAG_END_OF_MIB_VIEW):
        return tlv(tag, b"")
    if tag == TAG_INTEGER:
        return encode_integer(int(value))
    if tag == TAG_OCTET_STRING:
        return encode_octet_string(value)
    if tag == TAG_OID:
        return encode_oid(value)
    if tag in (0x41, 0x42, 0x43, 0x46):
        number = int(value)
        return tlv(tag, number.to_bytes(max(1, (number.bit_length() + 7) // 8), "big"))
    raise BerError(f"unsupported BER tag 0x{tag:02x}")


def encode_varbinds(varbinds: Iterable[Varbind], null_values: bool = False) -> bytes:
    encoded = []
    for varbind in varbinds:
        value = encode_null() if null_values else encode_value(varbind.tag, varbind.value)
        encoded.append(tlv(TAG_SEQUENCE, encode_oid(varbind.oid) + value))
    return tlv(TAG_SEQUENCE, b"".join(encoded))


def encode_message(
    community: str,
    pdu_tag: int,
    request_id: int,
    oids: Iterable[str | Sequence[int]],
    *,
    error_status: int = 0,
    error_index: int = 0,
    varbinds: Iterable[Varbind] | None = None,
) -> bytes:
    if varbinds is None:
        varbinds = [Varbind(oid_to_tuple(oid), TAG_NULL, None) for oid in oids]
    body = (
        encode_integer(request_id)
        + encode_integer(error_status)
        + encode_integer(error_index)
        + encode_varbinds(varbinds, null_values=(pdu_tag != TAG_GET_RESPONSE))
    )
    pdu = tlv(pdu_tag, body)
    return tlv(
        TAG_SEQUENCE,
        encode_integer(1) + encode_octet_string(community) + pdu,
    )


def decode_message(packet: bytes) -> SnmpMessage:
    tag, outer, end = read_tlv(packet, 0)
    if tag != TAG_SEQUENCE or end != len(packet):
        raise BerError("SNMP message is not a complete sequence")
    offset = 0
    tag, body, offset = read_tlv(outer, offset)
    if tag != TAG_INTEGER:
        raise BerError("missing SNMP version")
    version = decode_integer(body)
    tag, body, offset = read_tlv(outer, offset)
    if tag != TAG_OCTET_STRING:
        raise BerError("missing SNMP community")
    community = body.decode("utf-8", errors="replace")
    tag, body, offset = read_tlv(outer, offset)
    if tag not in (
        TAG_GET_REQUEST,
        TAG_GET_NEXT_REQUEST,
        TAG_GET_RESPONSE,
        TAG_SET_REQUEST,
        TAG_GET_BULK_REQUEST,
    ):
        raise BerError(f"unsupported SNMP PDU 0x{tag:02x}")
    pdu_tag = tag
    pdu_offset = 0
    tag, request_body, pdu_offset = read_tlv(body, pdu_offset)
    if tag != TAG_INTEGER:
        raise BerError("missing request id")
    request_id = decode_integer(request_body)
    tag, error_body, pdu_offset = read_tlv(body, pdu_offset)
    if tag != TAG_INTEGER:
        raise BerError("missing error status")
    error_status = decode_integer(error_body)
    tag, index_body, pdu_offset = read_tlv(body, pdu_offset)
    if tag != TAG_INTEGER:
        raise BerError("missing error index")
    error_index = decode_integer(index_body)
    tag, list_body, pdu_offset = read_tlv(body, pdu_offset)
    if tag != TAG_SEQUENCE:
        raise BerError("missing varbind list")
    varbind_offset = 0
    values: list[Varbind] = []
    while varbind_offset < len(list_body):
        tag, vb_body, varbind_offset = read_tlv(list_body, varbind_offset)
        if tag != TAG_SEQUENCE:
            raise BerError("invalid varbind")
        inner = 0
        tag, oid_body, inner = read_tlv(vb_body, inner)
        if tag != TAG_OID:
            raise BerError("missing varbind OID")
        oid, _ = decode_oid(oid_body)
        tag, value_body, inner = read_tlv(vb_body, inner)
        values.append(Varbind(oid, tag, decode_value(tag, value_body)))
    return SnmpMessage(
        version=version,
        community=community,
        pdu_tag=pdu_tag,
        request_id=request_id,
        error_status=error_status,
        error_index=error_index,
        varbinds=tuple(values),
    )
