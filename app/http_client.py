from __future__ import annotations

import json
import socket
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any


_LOCAL = threading.local()


def _opener() -> urllib.request.OpenerDirector:
    opener = getattr(_LOCAL, "opener", None)
    if opener is None:
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        _LOCAL.opener = opener
    return opener


@dataclass(frozen=True)
class HttpOutcome:
    success: bool
    status_code: int | None
    body: str
    kind: str
    message: str
    timed_out: bool
    permanent: bool
    latency_ms: float


def _read_body(response: Any) -> str:
    try:
        return response.read(8192).decode("utf-8", errors="replace")
    except Exception:
        return ""


def _is_timeout(reason: Any) -> bool:
    return isinstance(reason, (socket.timeout, TimeoutError)) or "timed out" in str(
        reason
    ).lower()


def post_event(
    receiver_url: str,
    *,
    event_id: str,
    tenant_id: str,
    payload: Any,
    attempt: int,
    timeout_ms: int,
) -> HttpOutcome:
    body = json.dumps(
        {
            "event_id": event_id,
            "tenant_id": tenant_id,
            "attempt": attempt,
            "payload": payload,
        },
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
    ).encode("utf-8")
    request = urllib.request.Request(
        f"{receiver_url.rstrip('/')}/webhook",
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
            "Idempotency-Key": event_id,
            "X-Event-ID": event_id,
            "X-Tenant-ID": tenant_id,
        },
    )
    started = time.monotonic()
    try:
        with _opener().open(
            request, timeout=max(0.001, timeout_ms / 1000.0)
        ) as response:
            latency_ms = (time.monotonic() - started) * 1000.0
            status_code = int(response.status)
            response_body = _read_body(response)
            if 200 <= status_code < 300:
                return HttpOutcome(
                    success=True,
                    status_code=status_code,
                    body=response_body,
                    kind="success",
                    message="",
                    timed_out=False,
                    permanent=False,
                    latency_ms=latency_ms,
                )
            return HttpOutcome(
                success=False,
                status_code=status_code,
                body=response_body,
                kind=f"http_{status_code}",
                message=f"HTTP {status_code}",
                timed_out=False,
                permanent=400 <= status_code < 500,
                latency_ms=latency_ms,
            )
    except urllib.error.HTTPError as exc:
        latency_ms = (time.monotonic() - started) * 1000.0
        status_code = int(exc.code)
        response_body = _read_body(exc)
        exc.close()
        return HttpOutcome(
            success=False,
            status_code=status_code,
            body=response_body,
            kind=f"http_{status_code}",
            message=f"HTTP {status_code}",
            timed_out=False,
            permanent=400 <= status_code < 500,
            latency_ms=latency_ms,
        )
    except urllib.error.URLError as exc:
        latency_ms = (time.monotonic() - started) * 1000.0
        timed_out = _is_timeout(exc.reason)
        return HttpOutcome(
            success=False,
            status_code=None,
            body="",
            kind="timeout" if timed_out else "connection_error",
            message=(
                f"timeout after {timeout_ms}ms"
                if timed_out
                else f"connection error: {exc.reason}"
            ),
            timed_out=timed_out,
            permanent=False,
            latency_ms=latency_ms,
        )
    except (socket.timeout, TimeoutError):
        latency_ms = (time.monotonic() - started) * 1000.0
        return HttpOutcome(
            success=False,
            status_code=None,
            body="",
            kind="timeout",
            message=f"timeout after {timeout_ms}ms",
            timed_out=True,
            permanent=False,
            latency_ms=latency_ms,
        )
    except Exception as exc:
        latency_ms = (time.monotonic() - started) * 1000.0
        return HttpOutcome(
            success=False,
            status_code=None,
            body="",
            kind="connection_error",
            message=f"connection error: {exc}",
            timed_out=False,
            permanent=False,
            latency_ms=latency_ms,
        )


def get_json(url: str, *, timeout_ms: int = 2000) -> tuple[int | None, Any, str]:
    request = urllib.request.Request(
        url, method="GET", headers={"Accept": "application/json"}
    )
    try:
        with _opener().open(
            request, timeout=max(0.001, timeout_ms / 1000.0)
        ) as response:
            text = _read_body(response)
            try:
                value = json.loads(text) if text else None
            except json.JSONDecodeError:
                value = None
            return int(response.status), value, ""
    except urllib.error.HTTPError as exc:
        text = _read_body(exc)
        exc.close()
        try:
            value = json.loads(text) if text else None
        except json.JSONDecodeError:
            value = None
        return int(exc.code), value, f"HTTP {exc.code}"
    except Exception as exc:
        return None, None, str(exc)


def post_json(
    url: str, value: Any, *, timeout_ms: int = 2000
) -> tuple[int | None, Any, str]:
    body = json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={"Content-Type": "application/json", "Accept": "application/json"},
    )
    try:
        with _opener().open(
            request, timeout=max(0.001, timeout_ms / 1000.0)
        ) as response:
            text = _read_body(response)
            try:
                response_value = json.loads(text) if text else None
            except json.JSONDecodeError:
                response_value = None
            return int(response.status), response_value, ""
    except urllib.error.HTTPError as exc:
        text = _read_body(exc)
        exc.close()
        try:
            response_value = json.loads(text) if text else None
        except json.JSONDecodeError:
            response_value = None
        return int(exc.code), response_value, f"HTTP {exc.code}"
    except Exception as exc:
        return None, None, str(exc)

