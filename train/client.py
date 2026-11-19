from __future__ import annotations

import time
from typing import Callable, TypeVar

import grpc

from proto import training_pb2_grpc as pb_grpc


T = TypeVar("T")


def make_channel(endpoint: str) -> grpc.Channel:
    return grpc.insecure_channel(
        endpoint,
        options=[
            ("grpc.max_send_message_length", 16 * 1024 * 1024),
            ("grpc.max_receive_message_length", 16 * 1024 * 1024),
            ("grpc.keepalive_time_ms", 2000),
            ("grpc.keepalive_timeout_ms", 1000),
            ("grpc.keepalive_permit_without_calls", 1),
        ],
    )


def coordinator_endpoint(config: dict) -> str:
    coordinator = config["coordinator"]
    return f"{coordinator['host']}:{coordinator['port']}"


def coordinator_stub(config: dict) -> tuple[grpc.Channel, pb_grpc.CoordinatorStub]:
    channel = make_channel(coordinator_endpoint(config))
    return channel, pb_grpc.CoordinatorStub(channel)


def retry_rpc(
    operation: Callable[[], T],
    *,
    timeout_seconds: float,
    description: str,
    stop_event=None,
) -> T:
    deadline = time.monotonic() + timeout_seconds
    delay = 0.05
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        if stop_event is not None and stop_event.is_set():
            break
        try:
            return operation()
        except grpc.RpcError as exc:
            last_error = exc
            code = exc.code() if hasattr(exc, "code") else None
            if code not in (
                grpc.StatusCode.UNAVAILABLE,
                grpc.StatusCode.DEADLINE_EXCEEDED,
                grpc.StatusCode.UNKNOWN,
                grpc.StatusCode.RESOURCE_EXHAUSTED,
            ):
                raise
        time.sleep(delay)
        delay = min(0.5, delay * 1.5)
    raise TimeoutError(f"retry deadline exceeded for {description}: {last_error}")
