"""Local, deterministic light-probe baking.

The public surface intentionally keeps the renderer-facing snapshot immutable:
queries pin one published version and never observe a partially rebuilt state.
"""

from .errors import ErrorCode, ProbeError
from .scene import (
    BoxOccluder,
    Material,
    Scene,
    Sky,
    SphereOccluder,
    load_scene,
    parse_scene,
)
from .grid import ProbeGrid, build_probe_grid
from .strategy import BakeConfig, StrategyDecision
from .bake import BakeEngine, BakeOutcome, BakeState
from .publish import IrradianceSnapshot, PublishedStore
from .snapshot import SnapshotStore

__all__ = [
    "BakeConfig",
    "BakeEngine",
    "BakeState",
    "BoxOccluder",
    "ErrorCode",
    "BakeOutcome",
    "IrradianceSnapshot",
    "Material",
    "ProbeError",
    "ProbeGrid",
    "PublishedStore",
    "Scene",
    "Sky",
    "SnapshotStore",
    "SphereOccluder",
    "StrategyDecision",
    "build_probe_grid",
    "load_scene",
    "parse_scene",
]

