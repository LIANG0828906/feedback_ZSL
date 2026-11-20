"""Deterministic scientific image-sequence processing workbench."""

from .errors import WorkbenchError
from .engine import ProcessResult, Workbench
from .factors import ErrorModel, FactorSet, NoiseParams
from .model import (
    ImageHeader,
    ImageSequence,
    PixelFormat,
    SequenceStructure,
    load_sequence,
    parse_sequence,
    read_sequence,
    write_sequence,
)

__all__ = [
    "ImageHeader",
    "ImageSequence",
    "PixelFormat",
    "ProcessResult",
    "SequenceStructure",
    "FactorSet",
    "NoiseParams",
    "ErrorModel",
    "Workbench",
    "WorkbenchError",
    "load_sequence",
    "parse_sequence",
    "read_sequence",
    "write_sequence",
]

__version__ = "0.1.0"
