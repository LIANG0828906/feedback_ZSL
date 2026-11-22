"""SpectraForge multispectral and HDR image composition workbench."""

from .errors import WorkbenchError
from .versions import ALGORITHM_VERSION, SCHEMA_VERSION

__all__ = ["WorkbenchError", "ALGORITHM_VERSION", "SCHEMA_VERSION"]
__version__ = "1.0.0"
