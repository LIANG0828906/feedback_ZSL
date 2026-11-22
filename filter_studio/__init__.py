"""Lumen Filter Studio core package."""

from .errors import WorkbenchError
from .pipeline import PipelineSession, RunControl
from .stages import DEFAULT_PARAMS, STAGE_IDS, normalize_pipeline_params

__all__ = [
    "DEFAULT_PARAMS",
    "STAGE_IDS",
    "PipelineSession",
    "RunControl",
    "WorkbenchError",
    "normalize_pipeline_params",
]

__version__ = "0.1.0"
