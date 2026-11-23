"""Spatial audio analysis, reconstruction and deterministic verification."""

from .config import AnalysisConfig, RenderConfig, WorkbenchConfig
from .errors import ErrorCode, WorkbenchError
from .generator import GeneratedDataset, GeneratedSample, generate_dataset, generate_demo
from .loader import load_analysis_profile, load_manifest, load_sample
from .pipeline import (
    ComparisonSummary,
    ProcessingSession,
    RunResult,
    compare_results,
    run_full_recompute,
    run_realtime,
    run_resume,
)

__all__ = [
    "AnalysisConfig",
    "ComparisonSummary",
    "ErrorCode",
    "GeneratedDataset",
    "GeneratedSample",
    "ProcessingSession",
    "RenderConfig",
    "RunResult",
    "WorkbenchConfig",
    "WorkbenchError",
    "compare_results",
    "generate_dataset",
    "generate_demo",
    "load_analysis_profile",
    "load_manifest",
    "load_sample",
    "run_full_recompute",
    "run_realtime",
    "run_resume",
]

__version__ = "1.0.0"
ALGORITHM_VERSION = "spatial-workbench-1.0.0"
