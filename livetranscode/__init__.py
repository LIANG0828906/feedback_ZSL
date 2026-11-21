"""Local multi-track slicing and adaptive-playback simulation."""

from .engine import TranscodeEngine
from .errors import BusinessError, ErrorCode

__all__ = ["BusinessError", "ErrorCode", "TranscodeEngine"]
__version__ = "1.0.0"

