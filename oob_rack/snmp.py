from __future__ import annotations

import os
import socket
import threading
import time

from .deterministic import SENSOR_SPECS
from .emulator_state import BmcRuntime
from .errors import PortInUseError, ProtocolError

SNMP_GET = 0xA0
SNMP_GETNEXT = 0xA1
SNMP_RESPONSE = 0xA2
SNMP_GETBULK = 0xA5

TAG_INTEGER = 0x02
TAG_OCTET_STRING = 0x04
TAG_NULL = 0x05
TAG_OID = 0x06
TAG_TIMETICKS = 0x43
TAG_GAUGE = 0x42
TAG_NO_SUCH_OBJECT = 0x80
TAG_END_OF_MIB = 0x82


def _encode_length(length: int) -> bytes:
    if length < 0x80:
        return bytes((length,))
    encoded = length.to_bytes((length.bit_length() + 7) // 8, "big")
    return bytes((0x80 | len(encoded),)) + encoded


def _tlv(tag: int, value: bytes) -> bytes:
    return bytes((tag,)) + _encode_length(len(value)) + value


def encode_integer(value: int) -> bytes:
    if value == 0:
        return b"\x00"
    if value > 0:
        raw = value.to_bytes((value.bit_length() + 7) // 8, "big")
        if raw[0] & 0x80:
            raw = b"\x00" + raw
        return raw
    length = max(1, ((~value).bit_length() + 8) // 8)
    raw = value.to_bytes(length, "big", signed=True)
    while len(raw) > 1 and raw[0] == 0xFF and raw[1] & 0x80:
        raw = raw[1:]
    return raw


def encode_oid(oid: str) -> bytes:
    parts = [int(part) for part in oid.strip(".").split(".")]
    if len(parts) < 2 or parts[0] > 2 or (parts[0] < 2 and parts[1] > 39):
        raise ValueError(f"invalid OID: {oid}")
    encoded = bytearray((parts[0] * 40 + parts[1],))
    for part in parts[2:]:
        if part < 0:
            raise ValueError(f"invalid OID component: {part}")
        stack = [part & 0x7F]
        part >>= 7
        while part:
            stack.append((part & 0x7F) | 0x80)
            part >>= 7
        encoded.extend(reversed(stack))
    return bytes(encoded)


def decode_oid(value: bytes) -> str:
    if not value:
        raise ValueError("empty OID")
    first = value[0]
    parts = [first // 40, first % 40]
    current = 0
    for byte in value[1:]:
        current = (current << 7) | (byte & 0x7F)
        if not byte & 0x80:
            parts.append(current)
            current = 0
    return ".".join(str(part) for part in parts)


def decode_integer(value: bytes) -> int:
    if not value:
        return 0
    return int.from_bytes(value, "big", signed=bool(value[0] & 0x80))


def read_tlv(data: bytes, offset: int = 0, *, strict: bool = True) -> tuple[int, bytes, int]:
    if offset >= len(data):
        raise ValueError("truncated BER tag")
    tag = data[offset]
    offset += 1
    if offset >= len(data):
        raise ValueError("truncated BER length")
    first = data[offset]
    offset += 1
    if first & 0x80:
        count = first & 0x7F
        if count == 0:
            if strict:
                raise ValueError("indefinite BER length is not supported")
            end = data.find(b"\x00\x00", offset)
            if end < 0:
                raise ValueError("unterminated BER value")
            return tag, data[offset:end], end + 2
        if count > 4 or offset + count > len(data):
            raise ValueError("invalid BER length")
        length = int.from_bytes(data[offset : offset + count], "big")
        offset += count
    else:
        length = first
    end = offset + length
    if end > len(data):
        raise ValueError("truncated BER value")
    return tag, data[offset:end], end


def _encode_value(value) -> tuple[int, bytes]:
    if isinstance(value, tuple):
        tag, raw = value
        return tag, raw
    if isinstance(value, bool):
        return TAG_INTEGER, encode_integer(1 if value else 0)
    if isinstance(value, int):
        return TAG_INTEGER, encode_integer(value)
    if isinstance(value, str):
        return TAG_OCTET_STRING, value.encode("utf-8")
    if isinstance(value, bytes):
        return TAG_OCTET_STRING, value
    raise TypeError(f"unsupported SNMP value: {type(value)!r}")


def _encode_varbind(oid: str, value_type: int | None = None, raw: bytes | None = None) -> bytes:
    if value_type is None:
        value = _tlv(TAG_NULL, b"")
    else:
        value = _tlv(value_type, raw or b"")
    return _tlv(0x30, _tlv(TAG_OID, encode_oid(oid)) + value)


def encode_snmp_message(
    community: str,
    request_id: int,
    varbinds: list[tuple[str, int | None, bytes | None]],
    *,
    error_status: int = 0,
    error_index: int = 0,
) -> bytes:
    pdu_body = (
        _tlv(TAG_INTEGER, encode_integer(request_id))
        + _tlv(TAG_INTEGER, encode_integer(error_status))
        + _tlv(TAG_INTEGER, encode_integer(error_index))
        + _tlv(0x30, b"".join(_encode_varbind(*item) for item in varbinds))
    )
    return _tlv(
        0x30,
        _tlv(TAG_INTEGER, encode_integer(1))
        + _tlv(TAG_OCTET_STRING, community.encode("utf-8"))
        + _tlv(SNMP_RESPONSE, pdu_body),
    )


def parse_snmp_message(data: bytes) -> tuple[int, str, int, int, list[tuple[str, int | None, bytes | None]]]:
    tag, body, end = read_tlv(data, 0)
    if tag != 0x30 or end != len(data):
        raise ValueError("invalid SNMP message")
    version_tag, version_raw, offset = read_tlv(body, 0)
    community_tag, community_raw, offset = read_tlv(body, offset)
    pdu_tag, pdu_raw, offset = read_tlv(body, offset)
    if version_tag != TAG_INTEGER or community_tag != TAG_OCTET_STRING:
        raise ValueError("invalid SNMP header")
    req_tag, req_raw, pdu_offset = read_tlv(pdu_raw, 0)
    err_tag, err_raw, pdu_offset = read_tlv(pdu_raw, pdu_offset)
    idx_tag, idx_raw, pdu_offset = read_tlv(pdu_raw, pdu_offset)
    list_tag, list_raw, pdu_offset = read_tlv(pdu_raw, pdu_offset)
    if req_tag != TAG_INTEGER or err_tag != TAG_INTEGER or idx_tag != TAG_INTEGER or list_tag != 0x30:
        raise ValueError("invalid SNMP PDU")
    varbinds = []
    cursor = 0
    while cursor < len(list_raw):
        vb_tag, vb_raw, cursor = read_tlv(list_raw, cursor)
        if vb_tag != 0x30:
            raise ValueError("invalid SNMP varbind")
        oid_tag, oid_raw, vb_offset = read_tlv(vb_raw, 0)
        val_tag, val_raw, vb_offset = read_tlv(vb_raw, vb_offset)
        if oid_tag != TAG_OID:
            raise ValueError("invalid SNMP varbind OID")
        varbinds.append((decode_oid(oid_raw), val_tag, val_raw))
    return (
        decode_integer(version_raw),
        community_raw.decode("utf-8", errors="replace"),
        pdu_tag,
        decode_integer(req_raw),
        varbinds,
    )


def _snmp_value_for_runtime(runtime: BmcRuntime, base: str) -> dict[str, object]:
    asset = runtime.asset
    power = runtime.power("snmp")
    values: dict[str, object] = {
        "1.3.6.1.2.1.1.1.0": f"OOB Rack Simulation {runtime.firmware}",
        "1.3.6.1.2.1.1.2.0": (TAG_OID, encode_oid(f"{base}.1")),
        "1.3.6.1.2.1.1.3.0": (TAG_TIMETICKS, encode_integer(int(time.monotonic() * 100) & 0xFFFFFFFF)),
        "1.3.6.1.2.1.1.4.0": "oob-simulation@example.invalid",
        "1.3.6.1.2.1.1.5.0": runtime.device_id,
        "1.3.6.1.2.1.1.6.0": asset["rack_id"],
        "1.3.6.1.2.1.1.7.0": 72,
        f"{base}.1.1.0": asset["manufacturer"],
        f"{base}.1.2.0": asset["model"],
        f"{base}.1.3.0": asset["serial"],
        f"{base}.1.4.0": asset["firmware_version"],
        f"{base}.1.5.0": asset["rack_id"],
        f"{base}.2.0": power,
        f"{base}.4.0": f"OOB-SIM-{runtime.device_id}",
    }
    spec_by_name = {spec.name: spec for spec in SENSOR_SPECS}
    sensors = runtime.sensors("snmp")
    for index, spec in enumerate(SENSOR_SPECS, start=1):
        value = sensors[spec.name]
        values[f"{base}.3.1.1.1.{index}"] = spec.name
        values[f"{base}.3.1.1.2.{index}"] = spec.kind
        values[f"{base}.3.1.1.3.{index}"] = int(value)
        values[f"{base}.3.1.1.4.{index}"] = spec.unit
        values[f"{base}.3.1.1.5.{index}"] = int(spec.lower)
        values[f"{base}.3.1.1.6.{index}"] = int(spec.upper)
    return values


def oid_key(oid: str) -> tuple[int, ...]:
    return tuple(int(part) for part in oid.split("."))


class SnmpEndpoint:
    def __init__(self, runtime: BmcRuntime, community: str, enterprise_base: str):
        self.runtime = runtime
        self.community = community
        self.enterprise_base = enterprise_base

    def handle(self, data: bytes) -> bytes | None:
        if self.runtime.is_offline("snmp"):
            return None
        try:
            version, community, pdu_tag, request_id, varbinds = parse_snmp_message(data)
        except (ValueError, IndexError):
            return None
        if community != self.community or version != 1:
            return None
        values = _snmp_value_for_runtime(self.runtime, self.enterprise_base)
        sorted_oids = sorted(values, key=oid_key)
        response: list[tuple[str, int | None, bytes | None]] = []
        if pdu_tag == SNMP_GET:
            for oid, _tag, _raw in varbinds:
                if oid in values:
                    tag, raw = _encode_value(values[oid])
                    response.append((oid, tag, raw))
                else:
                    response.append((oid, TAG_NO_SUCH_OBJECT, b""))
        elif pdu_tag == SNMP_GETNEXT:
            for oid, _tag, _raw in varbinds:
                target = next((candidate for candidate in sorted_oids if oid_key(candidate) > oid_key(oid)), None)
                if target is None:
                    response.append((oid, TAG_END_OF_MIB, b""))
                else:
                    tag, raw = _encode_value(values[target])
                    response.append((target, tag, raw))
        elif pdu_tag == SNMP_GETBULK:
            if len(varbinds) < 2:
                return None
            start = decode_integer(varbinds[0][2]) if varbinds[0][1] == TAG_INTEGER else 0
            repetitions = decode_integer(varbinds[1][2]) if varbinds[1][1] == TAG_INTEGER else 0
            repetitions = max(0, min(repetitions, 64))
            for oid, _tag, _raw in varbinds[2:]:
                current = oid
                for _ in range(repetitions):
                    target = next(
                        (candidate for candidate in sorted_oids if oid_key(candidate) > oid_key(current)),
                        None,
                    )
                    if target is None:
                        response.append((current, TAG_END_OF_MIB, b""))
                        break
                    tag, raw = _encode_value(values[target])
                    response.append((target, tag, raw))
                    current = target
            _ = start
        else:
            return None
        return encode_snmp_message(self.community, request_id, response)


class SnmpListener:
    def __init__(self, endpoint: SnmpEndpoint, host: str, port: int):
        self.endpoint = endpoint
        self.host = host
        self.port = port
        self.socket = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            self.socket.bind((host, port))
        except OSError as exc:
            self.socket.close()
            raise PortInUseError(
                f"cannot bind SNMP UDP {host}:{port}",
                details={"transport": "udp", "host": host, "port": port},
            ) from exc
        self.socket.settimeout(0.25)
        self._stop = threading.Event()
        self.thread = threading.Thread(
            target=self._run,
            name=f"snmp-{endpoint.runtime.device_id}",
            daemon=True,
        )
        self.started = False

    def start(self) -> None:
        self.thread.start()
        self.started = True

    def _run(self) -> None:
        while not self._stop.is_set():
            try:
                data, address = self.socket.recvfrom(65535)
            except socket.timeout:
                continue
            except OSError:
                break
            response = self.endpoint.handle(data)
            if response:
                try:
                    self.socket.sendto(response, address)
                except OSError:
                    pass

    def stop(self) -> None:
        self._stop.set()
        self.socket.close()
        if self.started:
            self.thread.join(timeout=2)
        self.started = False


class SnmpClient:
    def __init__(
        self,
        host: str,
        port: int,
        community: str,
        enterprise_base: str,
        *,
        timeout: float = 0.8,
        retries: int = 1,
    ):
        self.address = (host, port)
        self.community = community
        self.base = enterprise_base
        self.timeout = timeout
        self.retries = retries
        self.socket: socket.socket | None = None

    def __enter__(self):
        self.socket = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.socket.connect(self.address)
        self.socket.settimeout(self.timeout)
        return self

    def __exit__(self, exc_type, exc, traceback):
        if self.socket is not None:
            self.socket.close()
            self.socket = None
        return False

    def _request(self, pdu_tag: int, rows: list[tuple[str, int | None, bytes | None]]) -> list[tuple[str, int, bytes]]:
        if self.socket is None:
            raise ProtocolError("snmp", "client is not open")
        last_error: Exception | None = None
        for attempt in range(self.retries + 1):
            request_id = int.from_bytes(os.urandom(3), "big") or 1
            body = (
                _tlv(TAG_INTEGER, encode_integer(request_id))
                + _tlv(TAG_INTEGER, encode_integer(0))
                + _tlv(TAG_INTEGER, encode_integer(0))
                + _tlv(0x30, b"".join(_encode_varbind(*row) for row in rows))
            )
            packet = _tlv(
                0x30,
                _tlv(TAG_INTEGER, encode_integer(1))
                + _tlv(TAG_OCTET_STRING, self.community.encode())
                + _tlv(pdu_tag, body),
            )
            try:
                self.socket.send(packet)
                data, _address = self.socket.recvfrom(65535)
                version, community, response_tag, response_id, varbinds = parse_snmp_message(data)
                if version != 1 or response_tag != SNMP_RESPONSE or response_id != request_id:
                    raise ValueError("unexpected SNMP response")
                return [(oid, tag, raw) for oid, tag, raw in varbinds if tag is not None]
            except (OSError, ValueError) as exc:
                last_error = exc
                if attempt < self.retries:
                    time.sleep(0.03)
        raise ProtocolError("snmp", f"request failed: {last_error}")

    def get(self, oids: list[str]) -> dict[str, tuple[int, bytes]]:
        rows = [(oid, None, None) for oid in oids]
        return {
            oid: (tag, raw)
            for oid, tag, raw in self._request(SNMP_GET, rows)
        }

    def walk(self, root: str) -> dict[str, tuple[int, bytes]]:
        result: dict[str, tuple[int, bytes]] = {}
        current = root
        for _ in range(512):
            rows = self._request(SNMP_GETNEXT, [(current, None, None)])
            if not rows:
                break
            oid, tag, raw = rows[0]
            if tag == TAG_END_OF_MIB or not (oid_key(oid) > oid_key(root) and oid.startswith(root + ".")):
                break
            result[oid] = (tag, raw)
            current = oid
        return result

    def collect_payload(self) -> dict:
        asset_oids = {
            "manufacturer": f"{self.base}.1.1.0",
            "model": f"{self.base}.1.2.0",
            "serial": f"{self.base}.1.3.0",
            "firmware_version": f"{self.base}.1.4.0",
            "rack_id": f"{self.base}.1.5.0",
            "power": f"{self.base}.2.0",
        }
        values = self.get(list(asset_oids.values()))
        missing = [oid for oid in asset_oids.values() if oid not in values]
        if missing:
            raise ProtocolError("snmp", f"missing required OIDs: {missing}")

        def text(oid: str) -> str:
            tag, raw = values[oid]
            if tag != TAG_OCTET_STRING:
                raise ProtocolError("snmp", f"{oid} is not a string")
            return raw.decode("utf-8", errors="replace")

        walked = self.walk(f"{self.base}.3.1.1")
        columns: dict[int, dict[int, tuple[int, bytes]]] = {}
        prefix_parts = len(f"{self.base}.3.1.1".split("."))
        for oid, value in walked.items():
            parts = oid.split(".")
            if len(parts) != prefix_parts + 2:
                continue
            column = int(parts[-2])
            index = int(parts[-1])
            columns.setdefault(index, {})[column] = value
        sensors = []
        for index in sorted(columns):
            row = columns[index]
            try:
                name = row[1][1].decode("utf-8", errors="replace")
                kind = row[2][1].decode("utf-8", errors="replace")
                value = decode_integer(row[3][1])
                unit = row[4][1].decode("utf-8", errors="replace")
                lower = decode_integer(row[5][1])
                upper = decode_integer(row[6][1])
            except (KeyError, UnicodeDecodeError):
                continue
            sensors.append(
                {
                    "name": name,
                    "kind": kind,
                    "value": value,
                    "unit": unit,
                    "lower": lower,
                    "upper": upper,
                    "index": index,
                }
            )
        if not sensors:
            raise ProtocolError("snmp", "sensor table is empty")
        return {
            "asset": {
                "manufacturer": text(asset_oids["manufacturer"]),
                "model": text(asset_oids["model"]),
                "serial": text(asset_oids["serial"]),
                "firmware_version": text(asset_oids["firmware_version"]),
                "rack_id": text(asset_oids["rack_id"]),
            },
            "power_raw": {"text": text(asset_oids["power"])},
            "sensors": sensors,
            "raw": {
                "varbinds_hex": [
                    f"{oid}:{tag:02x}:{raw.hex()}" for oid, (tag, raw) in sorted(walked.items())
                ]
            },
        }
