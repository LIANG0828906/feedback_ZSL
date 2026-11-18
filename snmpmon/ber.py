"""Minimal BER codec sufficient for SNMPv2c (stdlib only)."""

TAG_INTEGER = 0x02
TAG_OCTET_STRING = 0x04
TAG_NULL = 0x05
TAG_OID = 0x06
TAG_SEQUENCE = 0x30
TAG_COUNTER32 = 0x41
TAG_GAUGE32 = 0x42
TAG_TIMETICKS = 0x43
TAG_NO_SUCH_OBJECT = 0x80
TAG_NO_SUCH_INSTANCE = 0x81
TAG_END_OF_MIB_VIEW = 0x82

PDU_GET = 0xA0
PDU_GETNEXT = 0xA1
PDU_RESPONSE = 0xA2

EXCEPTION_TAGS = (TAG_NO_SUCH_OBJECT, TAG_NO_SUCH_INSTANCE, TAG_END_OF_MIB_VIEW)


class BerError(ValueError):
    pass


def encode_length(n):
    if n < 0x80:
        return bytes([n])
    raw = n.to_bytes((n.bit_length() + 7) // 8, "big")
    return bytes([0x80 | len(raw)]) + raw


def encode_tlv(tag, content):
    return bytes([tag]) + encode_length(len(content)) + content


def encode_integer(value):
    length = 1
    while not (-(1 << (8 * length - 1)) <= value < (1 << (8 * length - 1))):
        length += 1
    return encode_tlv(TAG_INTEGER, value.to_bytes(length, "big", signed=True))


def encode_unsigned(tag, value):
    raw = value.to_bytes(max(1, (value.bit_length() + 7) // 8), "big")
    if raw[0] & 0x80:
        raw = b"\x00" + raw
    return encode_tlv(tag, raw)


def _encode_base128(n):
    if n == 0:
        return b"\x00"
    parts = []
    while n:
        parts.append(n & 0x7F)
        n >>= 7
    out = bytearray()
    for idx, part in enumerate(reversed(parts)):
        out.append(part | (0x80 if idx < len(parts) - 1 else 0))
    return bytes(out)


def encode_oid(oid):
    body = _encode_base128(40 * oid[0] + oid[1])
    for arc in oid[2:]:
        body += _encode_base128(arc)
    return encode_tlv(TAG_OID, body)


def encode_string(value):
    raw = value.encode("utf-8") if isinstance(value, str) else bytes(value)
    return encode_tlv(TAG_OCTET_STRING, raw)


def encode_null():
    return bytes([TAG_NULL, 0x00])


def encode_sequence(content):
    return encode_tlv(TAG_SEQUENCE, content)


def read_tlv(data, offset=0):
    if offset >= len(data):
        raise BerError("unexpected end of data")
    tag = data[offset]
    offset += 1
    if offset >= len(data):
        raise BerError("missing length")
    length = data[offset]
    offset += 1
    if length & 0x80:
        count = length & 0x7F
        if count == 0 or offset + count > len(data):
            raise BerError("bad long-form length")
        length = int.from_bytes(data[offset:offset + count], "big")
        offset += count
    if offset + length > len(data):
        raise BerError("content truncated")
    return tag, data[offset:offset + length], offset + length


def decode_integer(content):
    return int.from_bytes(content, "big", signed=True)


def decode_unsigned(content):
    return int.from_bytes(content, "big")


def decode_oid(content):
    subids = []
    value = 0
    for byte in content:
        value = (value << 7) | (byte & 0x7F)
        if not byte & 0x80:
            subids.append(value)
            value = 0
    if not subids:
        raise BerError("empty oid")
    first = subids[0]
    if first < 40:
        head = (0, first)
    elif first < 80:
        head = (1, first - 40)
    else:
        head = (2, first - 80)
    return tuple(head) + tuple(subids[1:])
