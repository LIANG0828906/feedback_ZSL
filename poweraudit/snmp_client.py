"""Concurrent, deadline-aware, read-only SNMP v2c polling."""

from __future__ import annotations

import secrets
import socket
import time
from dataclasses import dataclass
from typing import Any, Sequence

from .ber import (
    TAG_GET_REQUEST,
    BerError,
    decode_message,
    encode_message,
    oid_to_tuple,
)


class SnmpClientError(RuntimeError):
    pass


class SnmpTimeout(SnmpClientError):
    pass


class SnmpProtocolError(SnmpClientError):
    pass


class SnmpResponseError(SnmpClientError):
    def __init__(self, error_status: int, error_index: int) -> None:
        super().__init__(f"SNMP response error {error_status} at varbind {error_index}")
        self.error_status = error_status
        self.error_index = error_index


@dataclass(frozen=True)
class DeviceEndpoint:
    host: str
    port: int
    community: str


class SnmpClient:
    def __init__(self, *, max_varbinds_per_request: int = 24, retries: int = 0) -> None:
        self.max_varbinds_per_request = max(1, int(max_varbinds_per_request))
        self.retries = max(0, int(retries))

    def get(
        self,
        endpoint: DeviceEndpoint,
        oids: Sequence[str],
        *,
        timeout: float,
        deadline: float | None = None,
    ) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for offset in range(0, len(oids), self.max_varbinds_per_request):
            if deadline is not None and time.monotonic() >= deadline:
                raise SnmpTimeout("device deadline exceeded")
            chunk = list(oids[offset : offset + self.max_varbinds_per_request])
            response = self._get_once(endpoint, chunk, timeout=timeout, deadline=deadline)
            for varbind in response.varbinds:
                result[varbind.oid_text] = varbind.value
        return result

    def _get_once(
        self,
        endpoint: DeviceEndpoint,
        oids: Sequence[str],
        *,
        timeout: float,
        deadline: float | None,
    ) -> Any:
        request_id = secrets.randbits(31)
        packet = encode_message(
            endpoint.community, TAG_GET_REQUEST, request_id, oids
        )
        last_error: Exception | None = None
        for attempt in range(self.retries + 1):
            now = time.monotonic()
            attempt_deadline = now + timeout
            if deadline is not None:
                attempt_deadline = min(attempt_deadline, deadline)
            remaining = attempt_deadline - now
            if remaining <= 0:
                raise SnmpTimeout("device deadline exceeded")
            try:
                response = self._exchange(
                    endpoint, packet, request_id, remaining, attempt_deadline
                )
                if response.error_status:
                    raise SnmpResponseError(response.error_status, response.error_index)
                return response
            except SnmpTimeout as exc:
                last_error = exc
                continue
            except (OSError, SnmpProtocolError, BerError) as exc:
                last_error = exc
                if attempt >= self.retries:
                    break
        if isinstance(last_error, SnmpResponseError):
            raise last_error
        if isinstance(last_error, SnmpTimeout):
            raise last_error
        raise SnmpProtocolError(str(last_error or "SNMP request failed"))

    def _exchange(
        self,
        endpoint: DeviceEndpoint,
        packet: bytes,
        request_id: int,
        timeout: float,
        deadline: float,
    ) -> Any:
        family = socket.AF_INET6 if ":" in endpoint.host else socket.AF_INET
        with socket.socket(family, socket.SOCK_DGRAM) as sock:
            sock.settimeout(max(0.001, timeout))
            sock.sendto(packet, (endpoint.host, endpoint.port))
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise SnmpTimeout("SNMP response timeout")
                sock.settimeout(remaining)
                try:
                    data, _address = sock.recvfrom(65535)
                except socket.timeout as exc:
                    raise SnmpTimeout("SNMP response timeout") from exc
                try:
                    message = decode_message(data)
                except BerError as exc:
                    raise SnmpProtocolError(f"invalid SNMP response: {exc}") from exc
                if message.request_id != request_id:
                    continue
                if message.community != endpoint.community:
                    raise SnmpProtocolError("SNMP community mismatch in response")
                return message


def oid_for(base: str, suffix: str | int) -> str:
    return f"{base}.{suffix}"


def sorted_oid_values(values: dict[str, Any]) -> list[dict[str, Any]]:
    return [
        {"oid": oid, "value": values[oid]}
        for oid in sorted(values, key=oid_to_tuple)
    ]
