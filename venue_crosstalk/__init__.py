"""Deterministic sound-reinforcement crosstalk analysis workbench."""

from .analysis import analyze_band_coupling
from .calibration import calibrate_pair
from .config import AnalysisConfig, load_config
from .errors import (
    E_BUSY,
    E_CALIB_INFEASIBLE,
    E_GEOMETRY_INVALID,
    E_POLARITY_UNKNOWN,
    VenueError,
)
from .geometry import apply_geometry_alignment
from .pipeline import AnalysisPipeline

__all__ = [
    "AnalysisConfig",
    "AnalysisPipeline",
    "E_BUSY",
    "E_CALIB_INFEASIBLE",
    "E_GEOMETRY_INVALID",
    "E_POLARITY_UNKNOWN",
    "VenueError",
    "analyze_band_coupling",
    "apply_geometry_alignment",
    "calibrate_pair",
    "load_config",
]

__version__ = "1.0.0"
