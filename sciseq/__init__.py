"""Scientific imaging sequence processing toolkit."""

from .config import (
    ALGORITHM_VERSION,
    BudgetConfig,
    DenoiseConfig,
    PipelineConfig,
    RegistrationConfig,
)
from .errors import SciSeqError

__all__ = [
    "ALGORITHM_VERSION",
    "BudgetConfig",
    "DenoiseConfig",
    "PipelineConfig",
    "RegistrationConfig",
    "SciSeqError",
]

__version__ = "1.0.0"

