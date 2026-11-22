"""Deterministic spatial acoustic reconstruction workbench."""

from .errors import ErrorCode, WorkbenchError
from .models import AnalysisConfig, ChannelLayout, ChannelMap, FrameParameters
from .samplegen import generate_sample_bundle
from .io import load_sample
from .analysis import analyze_sample
from .render import RenderEngine
from .checkpoint import CheckpointStore
from .compare import compare_three_paths, verify_three_paths
from .faults import FaultPlan
from .pipeline import ReconstructionPipeline

__all__ = [
    "AnalysisConfig",
    "ChannelLayout",
    "ChannelMap",
    "CheckpointStore",
    "ErrorCode",
    "FaultPlan",
    "FrameParameters",
    "ReconstructionPipeline",
    "RenderEngine",
    "WorkbenchError",
    "analyze_sample",
    "compare_three_paths",
    "generate_sample_bundle",
    "load_sample",
    "verify_three_paths",
]

__version__ = "1.0.0"

