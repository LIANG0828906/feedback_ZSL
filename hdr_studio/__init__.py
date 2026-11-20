"""HDR delivery studio core package."""

from .errors import ErrorCode, HDRError
from .model import (
    ColorSpace,
    ConversionParams,
    ExposureSegment,
    ToneMap,
    TransferFunction,
)

__all__ = [
    "ColorSpace",
    "ConversionParams",
    "ErrorCode",
    "ExposureSegment",
    "HDRError",
    "ToneMap",
    "TransferFunction",
]

__version__ = "1.0.0"

