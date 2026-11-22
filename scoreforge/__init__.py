"""scoreforge - 面向游戏配乐与互动媒体的数字乐谱与演奏表达编辑器."""

from .errors import (
    ScoreError,
    SchemaError,
    RefError,
    DurationError,
    DanglingEventError,
    BatchError,
    CheckpointError,
    DivergenceError,
    PrecisionError,
)
from .units import TICKS_PER_QUARTER, PARAM_VERSION, ALGO_VERSION, DEFAULT_SEED

__all__ = [
    "ScoreError",
    "SchemaError",
    "RefError",
    "DurationError",
    "DanglingEventError",
    "BatchError",
    "CheckpointError",
    "DivergenceError",
    "PrecisionError",
    "TICKS_PER_QUARTER",
    "PARAM_VERSION",
    "ALGO_VERSION",
    "DEFAULT_SEED",
]

__version__ = "1.0.0"
