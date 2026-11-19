from __future__ import annotations

import hashlib
import hmac
import os
import socket
import struct
import time
from dataclasses import dataclass, field

from .deterministic import FAN_SPECS, SENSOR_SPECS, TEMPERATURE_SPECS
from .emulator_state import BmcRuntime
from .errors import ProtocolError

RMCP_HEADER = b"\x06\x00\xff\x07"
RMCP_AUTH_RMCP_PLUS = 0x06

PAYLOAD_IPMI = 0x00
PAYLOAD_OPEN_REQUEST = 0x10
PAYLOAD_OPEN_RESPONSE = 0x11
PAYLOAD_RAKP1 = 0x12
PAYLOAD_RAKP2 = 0x13
PAYLOAD_RAKP3 = 0x14
PAYLOAD_RAKP4 = 0x15

NETFN_CHASSIS = 0x00
NETFN_SENSOR_EVENT = 0x04
NETFN_APP = 0x06
NETFN_STORAGE = 0x0A

CMD_GET_CHASSIS_STATUS = 0x01
CMD_GET_SENSOR_READING = 0x2D
CMD_GET_SENSOR_THRESHOLDS = 0x27
CMD_GET_DEVICE_ID = 0x01
CMD_GET_SDR_REPO_INFO = 0x20
CMD_RESERVE_SDR_REPO = 0x22
CMD_GET_SDR = 0x23
CMD_GET_FRU_INFO = 0x10
CMD_READ_FRU = 0x11

AUTH_NONE = 0
AUTH_HMAC_SHA1 = 1
INTEGRITY_NONE = 0
CRYPT_NONE = 0

RAKP_NO_ERROR = 0x00
RAKP_INVALID_SESSION = 0x02
RAKP_INVALID_INTEGRITY = 0x0F
RAKP_NO_CIPHER = 0x11
RAKP_UNAUTHORIZED_NAME = 0x0D


def ipmi_checksum(data: bytes) -> int:
    return (-sum(data)) & 0xFF


def auth_key(password: str) -> bytes:
    encoded = password.encode("utf-8")
    return (encoded + b"\x00" * 20)[:20]


def hmac_sha1(key: bytes, data: bytes) -> bytes:
    return hmac.new(key, data, hashlib.sha1).digest()


def rmcp_packet(payload_type: int, payload: bytes, session_id: int = 0, sequence: int = 0) -> bytes:
    return (
        RMCP_HEADER
        + bytes((RMCP_AUTH_RMCP_PLUS, payload_type & 0x3F))
        + struct.pack("<IIH", session_id & 0xFFFFFFFF, sequence & 0xFFFFFFFF, len(payload))
        + payload
    )


def parse_rmcp_packet(data: bytes) -> tuple[int, int, int, bytes]:
    if len(data) < 16 or data[:4] != RMCP_HEADER or data[4] != RMCP_AUTH_RMCP_PLUS:
        raise ValueError("not an RMCP+ packet")
    payload_type = data[5] & 0x3F
    session_id, sequence, length = struct.unpack("<IIH", data[6:16])
    payload = data[16 : 16 + length]
    if len(payload) != length:
        raise ValueError("truncated RMCP+ payload")
    return payload_type, session_id, sequence, payload


def build_ipmi_request(netfn: int, cmd: int, data: bytes = b"", sequence: int = 0, lun: int = 0) -> bytes:
    first = bytes((0x20, ((netfn & 0x3F) << 2) | (lun & 0x03)))
    first += bytes((ipmi_checksum(first),))
    second = bytes((0x81, ((sequence & 0x3F) << 2) | (lun & 0x03), cmd & 0xFF)) + data
    second += bytes((ipmi_checksum(second),))
    return first + second


def parse_ipmi_request(message: bytes) -> tuple[int, int, int, int, bytes]:
    if len(message) < 7:
        raise ValueError("short IPMI request")
    if sum(message[0:3]) & 0xFF:
        raise ValueError("invalid IPMI request checksum")
    if sum(message[3:]) & 0xFF:
        raise ValueError("invalid IPMI request data checksum")
    netfn = message[1] >> 2
    sequence = message[4] >> 2
    lun = message[4] & 0x03
    cmd = message[5]
    return netfn, cmd, sequence, lun, message[6:-1]


def build_ipmi_response(
    netfn: int, cmd: int, sequence: int, lun: int, data: bytes = b"", completion: int = 0
) -> bytes:
    first = bytes((0x81, (((netfn | 1) & 0x3F) << 2) | (lun & 0x03)))
    first += bytes((ipmi_checksum(first),))
    second = bytes((0x20, ((sequence & 0x3F) << 2) | (lun & 0x03), cmd & 0xFF, completion)) + data
    second += bytes((ipmi_checksum(second),))
    return first + second


def parse_ipmi_response(message: bytes) -> tuple[int, int, int, bytes]:
    if len(message) < 7:
        raise ValueError("short IPMI response")
    if sum(message[0:3]) & 0xFF or sum(message[3:]) & 0xFF:
        raise ValueError("invalid IPMI response checksum")
    netfn = (message[1] >> 2) & 0x3F
    sequence = message[4] >> 2
    cmd = message[5]
    completion = message[6]
    return netfn, cmd, sequence, bytes((completion,)) + message[7:-1]


def _rakip_hmac_input_a(console_id: int, bmc_id: int, console_random: bytes, bmc_random: bytes, guid: bytes, role: int, username: bytes) -> bytes:
    return (
        struct.pack("<I", console_id)
        + struct.pack("<I", bmc_id)
        + console_random
        + bmc_random
        + guid
        + bytes((role & 0xFF, len(username) & 0xFF))
        + username
    )


def _rakp3_hmac_input(bmc_random: bytes, console_id: int, role: int, username: bytes) -> bytes:
    return (
        bmc_random
        + struct.pack("<I", console_id)
        + bytes((role & 0xFF, len(username) & 0xFF))
        + username
    )


def _sik_input(console_random: bytes, bmc_random: bytes, role: int, username: bytes) -> bytes:
    return (
        console_random
        + bmc_random
        + bytes((role & 0xFF, len(username) & 0xFF))
        + username
    )


def _rakp4_hmac_input(console_random: bytes, bmc_id: int, guid: bytes) -> bytes:
    return console_random + struct.pack("<I", bmc_id) + guid


@dataclass
class ServerSession:
    console_id: int
    bmc_id: int
    auth_alg: int = AUTH_NONE
    role: int = 4
    username: bytes = b""
    console_random: bytes = b""
    bmc_random: bytes = field(default_factory=lambda: os.urandom(16))
    guid: bytes = b""
    active: bool = False
    created_at: float = field(default_factory=time.monotonic)


class IpmiServerEndpoint:
    """RMCP+ endpoint for a single simulated BMC."""

    def __init__(self, runtime: BmcRuntime, username: str, password: str, guid: bytes):
        self.runtime = runtime
        self.username = username.encode("utf-8")
        self.password = password
        self.guid = guid
        self.key = auth_key(password)
        self.sessions: dict[int, ServerSession] = {}
        self.sensor_specs = SENSOR_SPECS
        self.sensor_numbers = {spec.name: index + 1 for index, spec in enumerate(SENSOR_SPECS)}
        self.sensor_records = [
            build_sensor_sdr(index + 1, index + 1, spec)
            for index, spec in enumerate(SENSOR_SPECS)
        ]
        asset = runtime.asset
        self.fru = build_fru(
            manufacturer=asset["manufacturer"],
            product=asset["model"],
            serial=asset["serial"],
            version=asset["firmware_version"],
        )

    def handle(self, packet: bytes) -> bytes | None:
        if self.runtime.is_offline("ipmi"):
            return None
        if (
            len(packet) >= 14
            and packet[:3] == RMCP_HEADER[:3]
            and packet[3] == 0x07
            and packet[4] != RMCP_AUTH_RMCP_PLUS
        ):
            return self._legacy_message(packet)
        try:
            payload_type, session_id, sequence, payload = parse_rmcp_packet(packet)
        except ValueError:
            return None
        if payload_type == PAYLOAD_OPEN_REQUEST:
            return self._open_session(payload)
        if payload_type == PAYLOAD_RAKP1:
            return self._rakp1(payload)
        if payload_type == PAYLOAD_RAKP3:
            return self._rakp3(payload)
        if payload_type == PAYLOAD_IPMI:
            return self._ipmi_message(session_id, sequence, payload)
        return None

    def _legacy_message(self, packet: bytes) -> bytes | None:
        try:
            length = packet[13]
            message = packet[14 : 14 + length]
            netfn, cmd, rq_seq, lun, data = parse_ipmi_request(message)
        except (ValueError, IndexError):
            return None
        completion, response_data = self._command(netfn, cmd, data)
        response = build_ipmi_response(netfn, cmd, rq_seq, lun, response_data, completion)
        return (
            RMCP_HEADER
            + bytes((0x00,))
            + bytes(4)
            + bytes(4)
            + bytes((len(response),))
            + response
        )

    def _open_session(self, payload: bytes) -> bytes:
        if len(payload) < 32:
            return b""
        tag = payload[0]
        console_id = int.from_bytes(payload[4:8], "little")
        auth_alg = payload[12]
        integrity_alg = payload[20]
        crypt_alg = payload[28]
        status = RAKP_NO_ERROR
        if auth_alg not in (AUTH_NONE, AUTH_HMAC_SHA1):
            status = 0x04
        elif integrity_alg != INTEGRITY_NONE or crypt_alg != CRYPT_NONE:
            status = RAKP_NO_CIPHER
        bmc_id = int.from_bytes(os.urandom(4), "little") or 1
        now = time.monotonic()
        for key, existing in list(self.sessions.items()):
            if now - existing.created_at > 120 or len(self.sessions) > 64:
                self.sessions.pop(key, None)
        if status == RAKP_NO_ERROR:
            self.sessions[console_id] = ServerSession(
                console_id=console_id,
                bmc_id=bmc_id,
                auth_alg=auth_alg,
                guid=self.guid,
            )
        response = (
            bytes((tag, status, 4, 0))
            + struct.pack("<I", console_id)
            + struct.pack("<I", bmc_id)
            + bytes((0, 0, 0, 8, auth_alg, 0, 0, 0))
            + bytes((1, 0, 0, 8, INTEGRITY_NONE, 0, 0, 0))
            + bytes((2, 0, 0, 8, CRYPT_NONE, 0, 0, 0))
        )
        return rmcp_packet(PAYLOAD_OPEN_RESPONSE, response)

    def _rakp1(self, payload: bytes) -> bytes:
        if len(payload) < 28:
            return b""
        tag = payload[0]
        bmc_id = int.from_bytes(payload[4:8], "little")
        session = next((value for value in self.sessions.values() if value.bmc_id == bmc_id), None)
        if session is None:
            return rmcp_packet(
                PAYLOAD_RAKP2,
                bytes((tag, RAKP_INVALID_SESSION, 0, 0)) + struct.pack("<I", 0),
            )
        console_random = bytes(payload[8:24])
        role = payload[24]
        username_length = payload[27]
        username = bytes(payload[28 : 28 + username_length])
        session.console_random = console_random
        session.role = role
        session.username = username
        if username != self.username:
            status = RAKP_UNAUTHORIZED_NAME
            mac = b""
        else:
            status = RAKP_NO_ERROR
            if session.auth_alg == AUTH_HMAC_SHA1:
                mac = hmac_sha1(
                    self.key,
                    _rakip_hmac_input_a(
                        session.console_id,
                        session.bmc_id,
                        console_random,
                        session.bmc_random,
                        self.guid,
                        role,
                        username,
                    ),
                )
            else:
                mac = b""
        response = (
            bytes((tag, status, 0, 0))
            + struct.pack("<I", session.console_id)
            + session.bmc_random
            + self.guid
            + mac
        )
        return rmcp_packet(PAYLOAD_RAKP2, response)

    def _rakp3(self, payload: bytes) -> bytes | None:
        if len(payload) < 8:
            return None
        tag = payload[0]
        status = payload[1]
        bmc_id = int.from_bytes(payload[4:8], "little")
        session = next((value for value in self.sessions.values() if value.bmc_id == bmc_id), None)
        if session is None:
            return None
        ok = status == RAKP_NO_ERROR
        if session.auth_alg == AUTH_HMAC_SHA1:
            expected = hmac_sha1(
                self.key,
                _rakp3_hmac_input(
                    session.bmc_random,
                    session.console_id,
                    session.role,
                    session.username,
                ),
            )
            ok = ok and hmac.compare_digest(expected, bytes(payload[8:28]))
        session.active = ok
        response_status = RAKP_NO_ERROR if ok else RAKP_INVALID_INTEGRITY
        mac = b""
        if ok and session.auth_alg == AUTH_HMAC_SHA1:
            sik = hmac_sha1(
                self.key,
                _sik_input(
                    session.console_random,
                    session.bmc_random,
                    session.role,
                    session.username,
                ),
            )
            mac = hmac_sha1(
                sik,
                _rakp4_hmac_input(session.console_random, session.bmc_id, self.guid),
            )[:12]
        response = (
            bytes((tag, response_status, 0, 0))
            + struct.pack("<I", session.console_id)
            + mac
        )
        # RAKP 3 is sent with a zero session ID; the response also uses zero.
        return rmcp_packet(PAYLOAD_RAKP4, response)

    def _find_active_session(self, bmc_id: int) -> ServerSession | None:
        return next(
            (value for value in self.sessions.values() if value.bmc_id == bmc_id and value.active),
            None,
        )

    def _ipmi_message(self, session_id: int, sequence: int, payload: bytes) -> bytes | None:
        session = self._find_active_session(session_id)
        if session is None:
            return None
        try:
            netfn, cmd, rq_seq, lun, data = parse_ipmi_request(payload)
        except ValueError:
            return None
        completion, response_data = self._command(netfn, cmd, data)
        response = build_ipmi_response(netfn, cmd, rq_seq, lun, response_data, completion)
        # ipmitool 1.8.x validates the response session ID against the
        # console session ID (not the BMC-side ID), so use the console ID on
        # established IPMI responses.
        return rmcp_packet(PAYLOAD_IPMI, response, session.console_id, sequence)

    def _command(self, netfn: int, cmd: int, data: bytes) -> tuple[int, bytes]:
        if netfn == NETFN_APP and cmd == CMD_GET_DEVICE_ID:
            firmware_parts = self.runtime.firmware.replace("legacy-", "").replace("mid-", "").replace("new-", "")
            try:
                major, minor = (int(part) for part in firmware_parts.split(".")[:2])
            except ValueError:
                major, minor = 1, 0
            response = bytes(
                (
                    0x20,
                    0x81,
                    major & 0xFF,
                    minor & 0xFF,
                    0x02,
                    0x0B,
                    0x00,
                    0x00,
                    0x00,
                    0x01,
                    0x00,
                    0x00,
                    0x00,
                    0x00,
                    0x00,
                )
            )
            return 0, response
        if netfn == NETFN_APP and cmd == 0x38:
            channel = (data[0] if data else 1) & 0x0F
            response = bytes(
                (
                    channel,
                    0x84,
                    0x04,
                    0x03,
                    0x00,
                    0x00,
                    0x00,
                    0x00,
                )
            )
            return 0, response
        if netfn == NETFN_APP and cmd == 0x3B:
            requested = data[0] if data else 0x04
            return 0, bytes((requested & 0x0F,))
        if netfn == NETFN_APP and cmd == 0x3C:
            return 0, b""
        if netfn == NETFN_CHASSIS and cmd == CMD_GET_CHASSIS_STATUS:
            power = self.runtime.power("ipmi")
            current = 0x01 if power == "on" else 0x00
            return 0, bytes((current, 0x00, 0x00))
        if netfn == NETFN_SENSOR_EVENT and cmd == CMD_GET_SENSOR_READING:
            if not data:
                return 0xC1, b""
            sensor_num = data[0]
            spec = next(
                (item for item in self.sensor_specs if self.sensor_numbers[item.name] == sensor_num),
                None,
            )
            if spec is None:
                return 0xCB, b""
            value = self.runtime.sensors()[spec.name]
            scale = 10 if spec.kind == "fan" else 1
            raw = max(0, min(255, int(round(float(value) / scale))))
            return 0, bytes((raw, 0x40, 0x00))
        if netfn == NETFN_SENSOR_EVENT and cmd == CMD_GET_SENSOR_THRESHOLDS:
            if not data:
                return 0xC1, b""
            spec = next(
                (item for item in self.sensor_specs if self.sensor_numbers[item.name] == data[0]),
                None,
            )
            if spec is None:
                return 0xCB, b""
            values = _sensor_raw_thresholds(spec)
            return 0, bytes((0x3F,)) + bytes(values)
        if netfn == NETFN_STORAGE and cmd == CMD_GET_SDR_REPO_INFO:
            count = len(self.sensor_records)
            response = (
                bytes((0x51,))
                + struct.pack("<H", count)
                + struct.pack("<H", 0x4000)
                + struct.pack("<I", 0)
                + struct.pack("<I", 0)
                + bytes((0x02,))
            )
            return 0, response
        if netfn == NETFN_STORAGE and cmd == CMD_RESERVE_SDR_REPO:
            return 0, struct.pack("<H", 1)
        if netfn == NETFN_STORAGE and cmd == CMD_GET_SDR:
            if len(data) < 6:
                return 0xC1, b""
            record_id = int.from_bytes(data[2:4], "little")
            offset = data[4]
            count = data[5]
            if record_id == 0:
                index = 0
            else:
                index = next(
                    (idx for idx, record in enumerate(self.sensor_records) if int.from_bytes(record[0:2], "little") == record_id),
                    -1,
                )
            if index < 0 or index >= len(self.sensor_records):
                return 0xCB, b""
            record = self.sensor_records[index]
            next_id = (
                int.from_bytes(self.sensor_records[index + 1][0:2], "little")
                if index + 1 < len(self.sensor_records)
                else 0xFFFF
            )
            chunk = record[offset : offset + count]
            return 0, struct.pack("<H", next_id) + chunk
        if netfn == NETFN_STORAGE and cmd == CMD_GET_FRU_INFO:
            if not data or data[0] != 0:
                return 0xCB, b""
            return 0, struct.pack("<H", len(self.fru)) + bytes((0x00,))
        if netfn == NETFN_STORAGE and cmd == CMD_READ_FRU:
            if len(data) < 4 or data[0] != 0:
                return 0xCB, b""
            offset = int.from_bytes(data[1:3], "little")
            count = data[3]
            chunk = self.fru[offset : offset + count]
            return 0, bytes((len(chunk),)) + chunk
        return 0xC1, b""


def build_sensor_sdr(record_id: int, sensor_number: int, spec) -> bytes:
    """Build a 64-byte IPMI full sensor record (M=1 for C, M=10 for RPM)."""
    payload = bytearray(59)
    payload[0] = 0x20
    payload[1] = 0x00
    payload[2] = sensor_number
    payload[3] = 0x07 if spec.kind == "temperature" else 0x1D
    payload[4] = min(sensor_number, 0x7F)
    payload[5] = 0x3F
    payload[6] = 0x00
    payload[7] = 0x01 if spec.kind == "temperature" else 0x04
    payload[8] = 0x01
    payload[13] = 0x3F
    payload[15] = 0x00
    payload[16] = 0x01 if spec.kind == "temperature" else 0x12
    payload[17] = 0x00
    payload[18] = 0x00
    scale = 10 if spec.kind == "fan" else 1
    payload[19:21] = struct.pack("<H", scale)
    payload[21:25] = bytes(4)
    payload[25] = 0x00
    payload[26] = 0
    payload[27] = 0
    payload[28] = 0
    payload[29] = 0xFF
    payload[30] = 0
    payload[31:37] = bytes(_sensor_raw_thresholds(spec))
    payload[37:39] = bytes(2)
    payload[39:41] = bytes(2)
    payload[41] = 0
    encoded_name = spec.name.encode("ascii")[:16]
    payload[42] = 0xC0 | len(encoded_name)
    payload[43 : 43 + len(encoded_name)] = encoded_name
    header = struct.pack("<HBB", record_id, 0x51, 0x01) + bytes((len(payload),))
    return header + bytes(payload)


def _sensor_raw_thresholds(spec) -> tuple[int, int, int, int, int, int]:
    scale = 10 if spec.kind == "fan" else 1

    def raw(value: float) -> int:
        return max(0, min(255, int(round(float(value) / scale))))

    upper = float(spec.upper)
    lower = float(spec.lower)
    return (
        raw(upper + 10),
        raw(upper + 5),
        raw(upper),
        raw(max(0.0, lower - 5)),
        raw(max(0.0, lower - 2)),
        raw(lower),
    )


def _fru_area(fields: list[tuple[int, str | bytes]], prefix: bytes = b"") -> bytes:
    area = bytearray((0x01, 0x00)) + bytearray(prefix)
    for field_type, value in fields:
        if isinstance(value, str):
            encoded = value.encode("ascii", errors="replace")[:63]
            type_bits = 0xC0
        else:
            encoded = value[:63]
            type_bits = 0x00
        area.append(type_bits | len(encoded))
        area.extend(encoded)
    area.append(0xC1)
    area.extend(b"\x00\x00")
    while len(area) % 8 != 0:
        area.append(0)
    area[1] = len(area) // 8
    area[-1] = ipmi_checksum(area[:-1])
    return bytes(area)


def build_fru(*, manufacturer: str, product: str, serial: str, version: str) -> bytes:
    chassis = _fru_area([(2, product), (2, serial)], prefix=b"\x11")
    board = _fru_area(
        [(2, manufacturer), (2, product), (2, serial), (2, product), (2, "OOB-SIM")],
        prefix=b"\x00\x00\x00\x00",
    )
    product_area = _fru_area(
        [
            (2, manufacturer),
            (2, product),
            (2, product),
            (2, version),
            (2, serial),
            (2, "OOB-SIM"),
            (2, "OOB-FRU"),
        ],
        prefix=b"\x00",
    )
    header = bytearray(8)
    header[0] = 0x01
    header[1] = 0x00
    header[2] = 1
    header[3] = 1 + len(chassis) // 8
    header[4] = 1 + (len(chassis) + len(board)) // 8
    header[5] = 0
    header[6] = 0
    header[7] = ipmi_checksum(header)
    return bytes(header) + chassis + board + product_area


def parse_fru(data: bytes) -> dict:
    if len(data) < 8 or data[0] != 0x01:
        raise ValueError("invalid FRU common header")
    if sum(data[:8]) & 0xFF:
        raise ValueError("invalid FRU header checksum")
    offsets = {"chassis": data[2] * 8, "board": data[3] * 8, "product": data[4] * 8}
    field_names = {
        "chassis": ["part_number", "serial"],
        "board": ["manufacturer", "model", "serial", "part_number", "file_id"],
        "product": [
            "manufacturer",
            "model",
            "part_number",
            "firmware_version",
            "serial",
            "asset_tag",
            "file_id",
        ],
    }
    header_skip = {"chassis": 3, "board": 6, "product": 3}
    parsed: dict[str, dict[str, str]] = {}
    for section, offset in offsets.items():
        if not offset:
            continue
        fields: dict[str, str] = {}
        if offset >= len(data):
            continue
        length = data[offset + 1] * 8 if offset + 1 < len(data) else 0
        end = min(len(data), offset + length) if length else len(data)
        cursor = offset + header_skip[section]
        index = 0
        while cursor + 1 < end:
            type_length = data[cursor]
            if type_length == 0xC1:
                break
            field_length = type_length & 0x3F
            field_type = (type_length >> 6) & 0x03
            cursor += 1
            raw = data[cursor : cursor + field_length]
            name = (
                field_names[section][index]
                if index < len(field_names[section])
                else f"field_{index}"
            )
            if field_type == 3:
                fields[name] = raw.decode("ascii", errors="replace").rstrip("\x00")
            else:
                fields[name] = raw.hex()
            cursor += field_length
            index += 1
        parsed[section] = fields
    product = parsed.get("product", {})
    board = parsed.get("board", {})
    return {
        "manufacturer": product.get("manufacturer") or board.get("manufacturer") or "",
        "model": product.get("model") or board.get("model") or "",
        "part_number": product.get("part_number") or board.get("part_number") or "",
        "firmware_version": product.get("firmware_version") or "",
        "serial": product.get("serial") or board.get("serial") or "",
        "asset_tag": product.get("asset_tag") or "",
        "raw_hex": data.hex(),
    }


class IpmiClient:
    def __init__(
        self,
        host: str,
        port: int,
        username: str,
        password: str,
        *,
        timeout: float = 0.8,
        retries: int = 1,
    ):
        self.address = (host, port)
        self.username = username.encode("utf-8")
        self.password = password
        self.key = auth_key(password)
        self.timeout = timeout
        self.retries = retries
        self.socket: socket.socket | None = None
        self.console_id = 0
        self.bmc_id = 0
        self.console_random = b""
        self.bmc_random = b""
        self.guid = b""
        self.auth_alg = AUTH_HMAC_SHA1
        self.sequence = 1

    def __enter__(self):
        self.open()
        return self

    def __exit__(self, exc_type, exc, traceback):
        self.close()
        return False

    def open(self) -> None:
        self.socket = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.socket.connect(self.address)
        self.socket.settimeout(self.timeout)
        self.console_id = int.from_bytes(os.urandom(4), "little") or 1
        open_payload = (
            bytes((0, 4, 0, 0))
            + struct.pack("<I", self.console_id)
            + bytes((0, 0, 0, 8, AUTH_HMAC_SHA1, 0, 0, 0))
            + bytes((1, 0, 0, 8, INTEGRITY_NONE, 0, 0, 0))
            + bytes((2, 0, 0, 8, CRYPT_NONE, 0, 0, 0))
        )
        packet_type, payload = self._exchange(PAYLOAD_OPEN_REQUEST, open_payload, (PAYLOAD_OPEN_RESPONSE,))
        if packet_type != PAYLOAD_OPEN_RESPONSE or len(payload) < 36:
            raise ProtocolError("ipmi", "invalid open-session response")
        status = payload[1]
        if status != 0:
            raise ProtocolError("ipmi", f"open-session rejected (0x{status:02x})")
        if int.from_bytes(payload[4:8], "little") != self.console_id:
            raise ProtocolError("ipmi", "console session id mismatch")
        self.bmc_id = int.from_bytes(payload[8:12], "little")
        self.auth_alg = payload[16]
        if payload[24] != INTEGRITY_NONE or payload[32] != CRYPT_NONE:
            raise ProtocolError("ipmi", "unsupported negotiated cipher suite")
        self.console_random = os.urandom(16)
        rakp1 = (
            bytes((0, 0, 0, 0))
            + struct.pack("<I", self.bmc_id)
            + self.console_random
            + bytes((4, 0, 0, len(self.username)))
            + self.username
        )
        packet_type, payload = self._exchange(PAYLOAD_RAKP1, rakp1, (PAYLOAD_RAKP2,))
        if packet_type != PAYLOAD_RAKP2 or len(payload) < 40:
            raise ProtocolError("ipmi", "invalid RAKP2 response")
        if payload[1] != 0:
            raise ProtocolError("ipmi", f"RAKP2 rejected (0x{payload[1]:02x})")
        self.bmc_random = bytes(payload[8:24])
        self.guid = bytes(payload[24:40])
        expected = hmac_sha1(
            self.key,
            _rakip_hmac_input_a(
                self.console_id,
                self.bmc_id,
                self.console_random,
                self.bmc_random,
                self.guid,
                4,
                self.username,
            ),
        )
        if not hmac.compare_digest(expected, bytes(payload[40:60])):
            raise ProtocolError("ipmi", "RAKP2 HMAC mismatch")
        rakp3_mac = hmac_sha1(
            self.key,
            _rakp3_hmac_input(self.bmc_random, self.console_id, 4, self.username),
        )
        rakp3 = bytes((0, 0, 0, 0)) + struct.pack("<I", self.bmc_id) + rakp3_mac
        packet_type, payload = self._exchange(PAYLOAD_RAKP3, rakp3, (PAYLOAD_RAKP4,))
        if packet_type != PAYLOAD_RAKP4 or len(payload) < 20:
            raise ProtocolError("ipmi", "invalid RAKP4 response")
        if payload[1] != 0:
            raise ProtocolError("ipmi", f"RAKP4 rejected (0x{payload[1]:02x})")
        sik = hmac_sha1(
            self.key,
            _sik_input(self.console_random, self.bmc_random, 4, self.username),
        )
        expected4 = hmac_sha1(
            sik, _rakp4_hmac_input(self.console_random, self.bmc_id, self.guid)
        )[:12]
        if not hmac.compare_digest(expected4, bytes(payload[8:20])):
            raise ProtocolError("ipmi", "RAKP4 HMAC mismatch")

    def close(self) -> None:
        if self.socket is not None:
            try:
                self.socket.close()
            finally:
                self.socket = None

    def _exchange(
        self,
        payload_type: int,
        payload: bytes,
        expected: tuple[int, ...],
        *,
        session_id: int = 0,
        sequence: int = 0,
    ):
        if self.socket is None:
            raise ProtocolError("ipmi", "session is not open")
        last_error: Exception | None = None
        for attempt in range(self.retries + 1):
            try:
                self.socket.send(
                    rmcp_packet(payload_type, payload, session_id=session_id, sequence=sequence)
                )
                data, _ = self.socket.recvfrom(65535)
                actual_type, _session_id, _sequence, actual_payload = parse_rmcp_packet(data)
                if actual_type not in expected:
                    raise ValueError(f"unexpected payload type 0x{actual_type:02x}")
                return actual_type, actual_payload
            except (OSError, ValueError) as exc:
                last_error = exc
                if attempt < self.retries:
                    time.sleep(0.03)
        raise ProtocolError("ipmi", f"request failed: {last_error}")

    def command(self, netfn: int, cmd: int, data: bytes = b"") -> bytes:
        message = build_ipmi_request(netfn, cmd, data, self.sequence)
        packet_type, payload = self._exchange(
            PAYLOAD_IPMI,
            message,
            (PAYLOAD_IPMI,),
            session_id=self.bmc_id,
            sequence=self.sequence,
        )
        if packet_type != PAYLOAD_IPMI:
            raise ProtocolError("ipmi", "unexpected IPMI response payload")
        _netfn, _cmd, _sequence, response = parse_ipmi_response(payload)
        completion = response[0]
        if completion:
            raise ProtocolError("ipmi", f"command 0x{cmd:02x} failed (completion 0x{completion:02x})")
        self.sequence = (self.sequence + 1) & 0x3F or 1
        return response[1:]

    def read_fru(self) -> bytes:
        info = self.command(NETFN_STORAGE, CMD_GET_FRU_INFO, b"\x00")
        if len(info) < 3:
            raise ProtocolError("ipmi", "short FRU inventory response")
        size = int.from_bytes(info[:2], "little")
        if size <= 0 or size > 65535:
            raise ProtocolError("ipmi", f"invalid FRU size {size}")
        result = bytearray()
        while len(result) < size:
            count = min(32, size - len(result))
            chunk = self.command(
                NETFN_STORAGE,
                CMD_READ_FRU,
                bytes((0,)) + struct.pack("<H", len(result)) + bytes((count,)),
            )
            if not chunk or chunk[0] == 0:
                break
            result.extend(chunk[1 : 1 + chunk[0]])
        return bytes(result)

    def read_sensor_records(self) -> list[bytes]:
        self.command(NETFN_APP, CMD_GET_DEVICE_ID)
        self.command(NETFN_STORAGE, CMD_RESERVE_SDR_REPO)
        records = []
        record_id = 0
        for _ in range(256):
            header = self.command(
                NETFN_STORAGE,
                CMD_GET_SDR,
                struct.pack("<HHBB", 1, record_id, 0, 5),
            )
            if len(header) < 7:
                break
            next_id = int.from_bytes(header[0:2], "little")
            length = header[6]
            body = bytearray()
            offset = 5
            while len(body) < length:
                count = min(60, length - len(body))
                part = self.command(
                    NETFN_STORAGE,
                    CMD_GET_SDR,
                    struct.pack("<HHBB", 1, record_id, offset + len(body), count),
                )
                if len(part) < 3:
                    break
                body.extend(part[2:])
            record = bytes(header[2:7]) + bytes(body)
            if len(record) >= 5 and record[3] == 0x01:
                records.append(record)
            if next_id == 0xFFFF:
                break
            record_id = next_id
        return records

    def collect_payload(self) -> dict:
        device_id = self.command(NETFN_APP, CMD_GET_DEVICE_ID)
        chassis = self.command(NETFN_CHASSIS, CMD_GET_CHASSIS_STATUS)
        power_byte = chassis[0] if chassis else 0
        fru_bytes = self.read_fru()
        fru = parse_fru(fru_bytes)
        sensor_payloads = []
        records_hex = []
        for record in self.read_sensor_records():
            if len(record) < 48:
                continue
            record_id = int.from_bytes(record[0:2], "little")
            payload = record[5:]
            sensor_num = payload[2]
            sensor_type = payload[7]
            unit_base = payload[16]
            mtol = int.from_bytes(payload[19:21], "little")
            scale = mtol & 0x03FF or 1
            id_code = payload[42]
            name_len = id_code & 0x1F
            name = payload[43 : 43 + name_len].decode("ascii", errors="replace")
            kind = "temperature" if sensor_type == 0x01 else "fan" if sensor_type == 0x04 else "other"
            if not name or kind == "other":
                continue
            reading = self.command(NETFN_SENSOR_EVENT, CMD_GET_SENSOR_READING, bytes((sensor_num,)))
            raw = reading[0] if reading else 0xFF
            value = None if raw == 0xFF else raw * scale
            records_hex.append(record.hex())
            sensor_payloads.append(
                {
                    "name": name,
                    "kind": kind,
                    "unit": "C" if unit_base == 0x01 else "RPM",
                    "sensor_number": sensor_num,
                    "raw_value": raw,
                    "scale": scale,
                    "value": value,
                }
            )
        return {
            "asset": {
                "manufacturer": fru.get("manufacturer", ""),
                "model": fru.get("model", ""),
                "serial": fru.get("serial", ""),
                "firmware_version": fru.get("firmware_version", ""),
                "device_id_hex": device_id.hex(),
                "fru_hex": fru_bytes.hex(),
            },
            "power_raw": {
                "byte": power_byte,
                "on": bool(power_byte & 0x01),
                "hex": chassis.hex(),
            },
            "sensors": sensor_payloads,
            "raw": {
                "device_id_hex": device_id.hex(),
                "chassis_status_hex": chassis.hex(),
                "fru_hex": fru_bytes.hex(),
                "sdr_records_hex": records_hex,
            },
        }
