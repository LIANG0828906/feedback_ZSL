"""Deterministic haptic event scheduling kernel.

The public API is intentionally small.  The modules underneath separate
validation, scheduling, encoding, transport, persistence, and diagnostics so
that each invariant can be tested independently.
"""

from .errors import ErrorCode, HapticError, Issue
from .models import (
    ChannelCapability,
    DeviceCapabilities,
    EncodedPacket,
    EventStream,
    HapticEvent,
    KernelVersions,
    PacketState,
    ScheduledPacket,
    ScheduledStream,
    ScheduleDecision,
)

KERNEL_VERSION = "1.0.0"

__all__ = [
    "KERNEL_VERSION",
    "ChannelCapability",
    "DeviceCapabilities",
    "EncodedPacket",
    "ErrorCode",
    "EventStream",
    "HapticError",
    "HapticEvent",
    "Issue",
    "KernelVersions",
    "PacketState",
    "ScheduleDecision",
    "ScheduledPacket",
    "ScheduledStream",
]

