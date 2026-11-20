"""Versioned tactical grid shortest-path service."""

from .changes import (
    SetBaseCost,
    SetDanger,
    SetObstacle,
    SetUnitPassability,
)
from .errors import (
    E_BATCH_FAILED,
    E_BUSY,
    E_RECOVERY_INVALID,
    E_VERSION_MISMATCH,
    ServiceError,
)
from .grid import WORLD_BLOCK_BIT, BattleGrid
from .persistence import SnapshotStore
from .service import (
    BatchStats,
    PathResult,
    PreparedBatch,
    TacticalService,
)

__all__ = [
    "E_BATCH_FAILED",
    "E_BUSY",
    "E_RECOVERY_INVALID",
    "E_VERSION_MISMATCH",
    "WORLD_BLOCK_BIT",
    "BatchStats",
    "BattleGrid",
    "PathResult",
    "PreparedBatch",
    "ServiceError",
    "SetBaseCost",
    "SetDanger",
    "SetObstacle",
    "SetUnitPassability",
    "SnapshotStore",
    "TacticalService",
]
