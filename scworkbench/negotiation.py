from __future__ import annotations

from .errors import NegotiationError
from .models import (
    CapabilitySet,
    NegotiationResult,
    SampleSession,
    capabilities_digest,
)
from .profiles import CODEC_PROFILES, PROTOCOL_VERSION


def _negotiate_resolution(
    declared_width: int,
    declared_height: int,
    sender: CapabilitySet,
    receiver: CapabilitySet,
) -> tuple[int, int]:
    width_limits = (declared_width, sender.max_width, receiver.max_width)
    height_limits = (declared_height, sender.max_height, receiver.max_height)
    scale = min(
        width_limits[0] / width_limits[0],
        width_limits[1] / declared_width,
        width_limits[2] / declared_width,
        height_limits[1] / declared_height,
        height_limits[2] / declared_height,
    )
    width = max(16, int(declared_width * scale))
    height = max(16, int(declared_height * scale))
    width = min(width, sender.max_width, receiver.max_width)
    height = min(height, sender.max_height, receiver.max_height)
    if width % 2:
        width -= 1
    if height % 2:
        height -= 1
    return max(16, width), max(16, height)


def negotiate(
    sample: SampleSession,
    sender_override: CapabilitySet | None = None,
    receiver_override: CapabilitySet | None = None,
) -> NegotiationResult:
    sender = sender_override or sample.sender.capabilities
    receiver = receiver_override or sample.receiver.capabilities
    missing_items: list[str] = []
    common = sorted(
        set(sender.codecs).intersection(receiver.codecs),
        key=lambda name: CODEC_PROFILES[name].rank,
        reverse=True,
    )
    if not common:
        missing_items.append("codecs")
    input_supported = sender.supports_input and receiver.supports_input
    if sample.requires_input_backchannel and not input_supported:
        missing_items.append("supports_input")
    if missing_items:
        raise NegotiationError(
            "双方能力集无可满足交集",
            items=missing_items,
            sender_codecs=list(sender.codecs),
            receiver_codecs=list(receiver.codecs),
            requires_input_backchannel=sample.requires_input_backchannel,
        )
    width, height = _negotiate_resolution(
        sample.declared_width, sample.declared_height, sender, receiver
    )
    if width < 16 or height < 16:
        raise NegotiationError(
            "双方能力集无可满足分辨率", items=["resolution"]
        )
    fps = max(1, min(sample.declared_fps, sender.max_fps, receiver.max_fps))
    return NegotiationResult(
        codec=common[0],
        width=width,
        height=height,
        fps=fps,
        supports_input=input_supported,
        sender_device_id=sample.sender_device_id,
        receiver_device_id=sample.receiver_device_id,
        capabilities_hash=capabilities_digest(
            sender, receiver, PROTOCOL_VERSION
        ),
    )
