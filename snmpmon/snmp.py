"""SNMPv2c message build/parse on top of the BER codec."""

from . import ber

VERSION_V2C = 1

VALUE_TYPE_NAMES = {
    ber.TAG_INTEGER: "Integer32",
    ber.TAG_OCTET_STRING: "OctetString",
    ber.TAG_OID: "ObjectIdentifier",
    ber.TAG_COUNTER32: "Counter32",
    ber.TAG_GAUGE32: "Gauge32",
    ber.TAG_TIMETICKS: "TimeTicks",
    ber.TAG_NULL: "Null",
}


class SnmpError(Exception):
    pass


def _encode_varbind(oid, tag, raw_value):
    return ber.encode_sequence(ber.encode_oid(oid) + ber.encode_tlv(tag, raw_value))


def build_request(pdu_tag, request_id, oids, community="public"):
    varbinds = b"".join(_encode_varbind(oid, ber.TAG_NULL, b"") for oid in oids)
    pdu = ber.encode_tlv(
        pdu_tag,
        ber.encode_integer(request_id)
        + ber.encode_integer(0)
        + ber.encode_integer(0)
        + ber.encode_sequence(varbinds),
    )
    return ber.encode_sequence(
        ber.encode_integer(VERSION_V2C) + ber.encode_string(community) + pdu
    )


def build_response(request_id, varbinds, community="public", version=VERSION_V2C):
    body = b"".join(_encode_varbind(oid, tag, raw) for oid, tag, raw in varbinds)
    pdu = ber.encode_tlv(
        ber.PDU_RESPONSE,
        ber.encode_integer(request_id)
        + ber.encode_integer(0)
        + ber.encode_integer(0)
        + ber.encode_sequence(body),
    )
    return ber.encode_sequence(
        ber.encode_integer(version) + ber.encode_string(community) + pdu
    )


def parse_message(data):
    tag, content, _ = ber.read_tlv(data, 0)
    if tag != ber.TAG_SEQUENCE:
        raise SnmpError("message is not a sequence")
    tag, raw, pos = ber.read_tlv(content, 0)
    if tag != ber.TAG_INTEGER:
        raise SnmpError("missing version")
    version = ber.decode_integer(raw)
    tag, community, pos = ber.read_tlv(content, pos)
    if tag != ber.TAG_OCTET_STRING:
        raise SnmpError("missing community")
    pdu_tag, pdu, _ = ber.read_tlv(content, pos)
    tag, raw, p = ber.read_tlv(pdu, 0)
    request_id = ber.decode_integer(raw)
    tag, raw, p = ber.read_tlv(pdu, p)
    error_status = ber.decode_integer(raw)
    tag, raw, p = ber.read_tlv(pdu, p)
    error_index = ber.decode_integer(raw)
    tag, varbind_list, _ = ber.read_tlv(pdu, p)
    varbinds = []
    cursor = 0
    while cursor < len(varbind_list):
        tag, varbind, cursor = ber.read_tlv(varbind_list, cursor)
        if tag != ber.TAG_SEQUENCE:
            raise SnmpError("varbind is not a sequence")
        tag, raw, vpos = ber.read_tlv(varbind, 0)
        oid = ber.decode_oid(raw)
        vtag, vraw, _ = ber.read_tlv(varbind, vpos)
        varbinds.append((oid, vtag, vraw))
    return {
        "version": version,
        "community": community.decode("utf-8", "replace"),
        "pdu": pdu_tag,
        "request_id": request_id,
        "error_status": error_status,
        "error_index": error_index,
        "varbinds": varbinds,
    }


def decode_value(tag, raw):
    if tag == ber.TAG_INTEGER:
        return ber.decode_integer(raw)
    if tag in (ber.TAG_COUNTER32, ber.TAG_GAUGE32, ber.TAG_TIMETICKS):
        return ber.decode_unsigned(raw)
    if tag == ber.TAG_OCTET_STRING:
        return raw.decode("utf-8", "replace")
    if tag == ber.TAG_OID:
        return ".".join(str(a) for a in ber.decode_oid(raw))
    if tag == ber.TAG_NULL:
        return None
    return None


def value_type_name(tag):
    return VALUE_TYPE_NAMES.get(tag, "Unknown")
