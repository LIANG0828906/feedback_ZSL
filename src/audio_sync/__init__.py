"""Multi-channel clock-drift synchronization workbench."""

from .errors import SyncError
from .models import SyncConfig
from .pipeline import run_pipeline
from .service import SyncRequest, SyncService

__all__ = ["SyncConfig", "SyncError", "SyncRequest", "SyncService", "run_pipeline"]
__version__ = "1.0.0"

