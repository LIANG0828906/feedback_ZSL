"""Deterministic score import, expression solving and versioned publishing."""

from .conversion import ConversionResult, convert_score, export_score, parse_score
from .errors import EngineError
from .model import Score, SolverParameters
from .publish import PublishedSnapshot, VersionedPublisher
from .solver import PerformanceSolver, SolverCancelled

__all__ = [
    "ConversionResult",
    "EngineError",
    "PerformanceSolver",
    "PublishedSnapshot",
    "Score",
    "SolverCancelled",
    "SolverParameters",
    "VersionedPublisher",
    "convert_score",
    "export_score",
    "parse_score",
]

__version__ = "0.1.0"

