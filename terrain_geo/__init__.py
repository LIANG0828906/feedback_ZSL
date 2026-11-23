"""Terrain texture geometric transform & distortion correction components."""

from .errors import GeoError, ErrorCode
from .stages import HomographyStage, RadialCorrectionStage, AffineStage, StageConfig
from .pipeline import Pipeline, PipelineResult
from .persistence import SnapshotStore
from .validation import validate_geometry
from .resample import ResampleConfig

SCHEMA_VERSION = "1.0.0"

__all__ = [
    "GeoError",
    "ErrorCode",
    "HomographyStage",
    "RadialCorrectionStage",
    "AffineStage",
    "StageConfig",
    "Pipeline",
    "PipelineResult",
    "SnapshotStore",
    "validate_geometry",
    "ResampleConfig",
    "SCHEMA_VERSION",
]
