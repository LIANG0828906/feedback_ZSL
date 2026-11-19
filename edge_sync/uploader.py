"""Shard upload client routed through the local egress proxies.

Each attempt opens a fresh TCP connection to the chosen egress proxy so the
congestion signals can be attributed to exactly one shard/connection.  Real
kernel readouts come from :mod:`edge_sync.tcp_probe`; the proxy's deterministic
link-emulation readouts arrive through the ``X-Link-Stats`` response header.
"""

from __future__ import annotations

import http.client
import json
import time

from . import ERR_HTTP, ERR_SHARD_TIMEOUT, ERR_SIGNAL_COLLECTION
from .tcp_probe import probe


class UploadResult:
    def __init__(
        self,
        success: bool,
        egress: str,
        started_ms: int,
        ended_ms: int,
        response: dict | None,
        signal: dict | None,
        link: dict | None,
        error_code: str | None,
        duplicate: bool = False,
    ):
        self.success = success
        self.egress = egress
        self.started_ms = started_ms
        self.ended_ms = ended_ms
        self.response = response
        self.signal = signal
        self.link = link
        self.error_code = error_code
        self.duplicate = duplicate

    def as_dict(self) -> dict:
        return {
            "success": self.success,
            "egress": self.egress,
            "started_ms": self.started_ms,
            "ended_ms": self.ended_ms,
            "response": self.response,
            "signal": self.signal,
            "link": self.link,
            "error_code": self.error_code,
            "duplicate": self.duplicate,
        }


def upload_shard(
    proxy_address: tuple[str, int],
    egress_id: str,
    shard: dict,
    payload: bytes,
    timeout_s: float,
) -> UploadResult:
    started_ms = int(time.time() * 1000)
    connection = None
    before = None
    try:
        connection = http.client.HTTPConnection(
            proxy_address[0], proxy_address[1], timeout=timeout_s
        )
        connection.connect()
        before = probe(connection.sock)
        headers = {
            "Host": f"edge-sync-target",
            "Transfer-Encoding": "chunked",
            "Content-Type": "application/octet-stream",
            "X-Task-Id": shard["task_id"],
            "X-Shard-Seq": str(shard["shard_seq"]),
            "X-Start-Byte": str(shard["start_byte"]),
            "X-End-Byte": str(shard["end_byte"]),
            "X-Checksum": shard["checksum"],
            "Connection": "close",
        }
        connection.putrequest("POST", "/ingest", skip_host=False, skip_accept_encoding=True)
        for key, value in headers.items():
            if key in ("Host", "Transfer-Encoding", "Connection"):
                continue
            connection.putheader(key, value)
        connection.putheader("Transfer-Encoding", "chunked")
        connection.putheader("Connection", "close")
        connection.endheaders()
        # Chunked upload body (HTTP/1.1 framing).
        offset = 0
        chunk_size = min(8192, len(payload))
        while offset < len(payload):
            block = payload[offset : offset + chunk_size]
            connection.send(b"%X\r\n" % len(block) + block + b"\r\n")
            offset += len(block)
        connection.send(b"0\r\n\r\n")
        # Probe while the socket is still owned by the client; reading the
        # response can return a closing socket on the fastest loopback runs.
        after = probe(connection.sock)
        response = connection.getresponse()
        body = response.read().decode("utf-8")
        parsed = json.loads(body) if body else {}
        link_header = response.getheader("X-Link-Stats")
        link = json.loads(link_header) if link_header else None
        signal = after.delta(before)
        ended_ms = int(time.time() * 1000)
        ok = response.status == 200 and "error" not in parsed
        if not ok and not signal.get("missing"):
            # Probe succeeded but the business call failed; keep both facts.
            pass
        return UploadResult(
            success=ok,
            egress=egress_id,
            started_ms=started_ms,
            ended_ms=ended_ms,
            response=parsed,
            signal=signal,
            link=link,
            error_code=None if ok else parsed.get("error", ERR_HTTP),
            duplicate=bool(parsed.get("duplicate")),
        )
    except TimeoutError:
        return _failure(egress_id, started_ms, connection, before, ERR_SHARD_TIMEOUT, None)
    except (OSError, http.client.HTTPException, json.JSONDecodeError) as exc:
        return _failure(egress_id, started_ms, connection, before, ERR_HTTP, str(exc))
    finally:
        if connection is not None:
            connection.close()


def _failure(
    egress_id: str,
    started_ms: int,
    connection,
    before,
    error_code: str,
    detail: str | None,
) -> UploadResult:
    signal = None
    link = None
    if connection is not None:
        try:
            after = probe(connection.sock)
            if before is not None:
                signal = after.delta(before)
        except Exception as exc:  # pragma: no cover - defensive boundary
            signal = {
                "missing": ["rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments"],
                "missing_codes": [ERR_SIGNAL_COLLECTION],
                "note": f"probe failure: {type(exc).__name__}: {exc}",
            }
    else:
        signal = {
            "missing": ["rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments"],
            "missing_codes": [ERR_SIGNAL_COLLECTION],
            "note": f"connection not established ({error_code})"
            + (f": {detail}" if detail else ""),
        }
    return UploadResult(
        success=False,
        egress=egress_id,
        started_ms=started_ms,
        ended_ms=int(time.time() * 1000),
        response=None,
        signal=signal,
        link=link,
        error_code=error_code,
    )
