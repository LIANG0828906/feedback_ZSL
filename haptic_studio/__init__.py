"""Haptic Studio deterministic waveform and versioning core."""

from .engine import RecomputePlan, execute_plan
from .errors import ErrorCode, HapticError
from .models import DeviceConfig, Envelope, WaveSegment, WaveSequence
from .sampler import SamplingResult, compute_full_signal
from .session import HapticSession

__all__ = [
    "DeviceConfig",
    "Envelope",
    "ErrorCode",
    "HapticError",
    "HapticSession",
    "RecomputePlan",
    "SamplingResult",
    "WaveSegment",
    "WaveSequence",
    "compute_full_signal",
    "execute_plan",
]
