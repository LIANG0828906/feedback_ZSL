"""Deterministic multichannel acoustic calibration workbench."""

from .alignment import align_impulse_responses, alignment_difference, detect_arrivals
from .engine import CalibrationEngine
from .errors import CalibrationError
from .filters import design_compensation_filter, smooth_band_powers
from .models import (
    AlignmentParams,
    CalibrationParams,
    CalibrationState,
    SpectralConfig,
    TargetCurve,
    ThresholdConfig,
)
from .propagation import build_error_report
from .samples import generate_sample_wav
from .spectral import estimate_channel_spectrum, make_bands
from .versioning import StateStore, version_diff
from .wavio import WavData, read_wav_16, write_wav_16

__all__ = [
    "AlignmentParams",
    "CalibrationEngine",
    "CalibrationError",
    "CalibrationParams",
    "CalibrationState",
    "SpectralConfig",
    "StateStore",
    "TargetCurve",
    "ThresholdConfig",
    "WavData",
    "align_impulse_responses",
    "alignment_difference",
    "build_error_report",
    "design_compensation_filter",
    "detect_arrivals",
    "estimate_channel_spectrum",
    "generate_sample_wav",
    "make_bands",
    "read_wav_16",
    "smooth_band_powers",
    "version_diff",
    "write_wav_16",
]
