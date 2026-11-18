"""Deterministic sharded-MLP parameter-server demonstration."""

import warnings

warnings.filterwarnings(
    "ignore",
    message=r"Failed to initialize NumPy: No module named 'numpy'.*",
    category=UserWarning,
)

__version__ = "1.0.0"
