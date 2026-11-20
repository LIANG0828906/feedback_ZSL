"""Deterministic, local broadcast loudness measurement workbench.

The package deliberately has no dependency on a GUI toolkit or a network
service.  The core can be driven from the CLI, tests, or an optional Tk UI.
"""

from .config import MeterConfig, WorkbenchConfig, load_config, load_workbench_config
from .budget import BudgetPlan
from .errors import ErrorCode, MeterError
from .metering import AssetMeter, analyze_offline, compare_reports
from .models import AssetReport, BlockResult, ResliceDiff
from .playback import PlaybackController, recover_from_snapshot
from .scheduler import MeterScheduler, SchedulerResult
from .snapshot import RecoveryManager, SnapshotStore

__all__ = [
    "AssetMeter",
    "AssetReport",
    "BlockResult",
    "BudgetPlan",
    "ErrorCode",
    "MeterConfig",
    "MeterError",
    "MeterScheduler",
    "PlaybackController",
    "RecoveryManager",
    "ResliceDiff",
    "SchedulerResult",
    "SnapshotStore",
    "WorkbenchConfig",
    "analyze_offline",
    "compare_reports",
    "load_config",
    "load_workbench_config",
    "recover_from_snapshot",
]

__version__ = "1.0.0"
