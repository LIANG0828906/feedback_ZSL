"""Deterministic differentiable rendering and inverse imaging components."""

from .errors import AppError
from .schema import Dataset, Task, load_dataset

__all__ = ["AppError", "Dataset", "Task", "load_dataset"]
__version__ = "1.0.0"

