"""Deterministic causal evaluation kernel for interactive narratives."""

from .engine import Engine, RunResult, replay_trace
from .errors import KernelError
from .precision import Precision
from .scenario import Scenario, load_scenario

__all__ = [
    "Engine",
    "KernelError",
    "Precision",
    "RunResult",
    "Scenario",
    "load_scenario",
    "replay_trace",
]

__version__ = "1.0.0"
