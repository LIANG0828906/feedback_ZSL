"""Deterministic multichannel acoustic calibration workbench."""

from .config import CalibrationConfig, TargetPoint
from .errors import CalibrationError
from .pipeline import CalibrationPipeline, ProcessResult

__all__ = [
    "CalibrationConfig",
    "CalibrationError",
    "CalibrationPipeline",
    "ProcessResult",
    "TargetPoint",
]

__version__ = "0.1.0"

