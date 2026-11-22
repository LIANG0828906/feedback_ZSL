"""Professional digital score & performance-expression editing workbench.

The package is deliberately split into pure-domain modules (no Qt dependency)
and a :mod:`scoreworkbench.gui` layer so that the whole business logic can be
exercised head-less with the standard ``unittest`` runner.
"""

from .errors import WorkbenchError
from .model import Score, Note, ExpressionEvent, TraceRecord, BatchRecord
from .constants import (
    DOMAIN_MODEL_VERSION,
    CONVERSION_RULES_VERSION,
    SUPPORTED_SCHEMA_VERSIONS,
)

__all__ = [
    "WorkbenchError",
    "Score",
    "Note",
    "ExpressionEvent",
    "TraceRecord",
    "BatchRecord",
    "DOMAIN_MODEL_VERSION",
    "CONVERSION_RULES_VERSION",
    "SUPPORTED_SCHEMA_VERSIONS",
]

__version__ = "1.0.0"
