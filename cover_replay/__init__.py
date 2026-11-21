"""Deterministic cover-durability replay for tactical match review."""

from .baseline import (
    build_baseline,
    compare_baseline,
    compare_strategies,
    compare_with_baseline_file,
    write_baseline,
)
from .config import ReplayConfig, load_config, parse_config
from .engine import ReplayEngine, ReplayRun, Withdrawal, replay
from .errors import BusinessError
from .generator import generate_samples
from .model import ReplayRecord, load_record, parse_record
from .snapshot import SnapshotStore
from .version import BASELINE_VERSION, MODEL_VERSION, SNAPSHOT_VERSION

__all__ = [
    "BusinessError",
    "ReplayConfig",
    "ReplayEngine",
    "ReplayRecord",
    "ReplayRun",
    "SnapshotStore",
    "Withdrawal",
    "build_baseline",
    "compare_baseline",
    "compare_strategies",
    "compare_with_baseline_file",
    "generate_samples",
    "load_config",
    "load_record",
    "parse_config",
    "parse_record",
    "replay",
    "write_baseline",
]

__version__ = "1.0.0"
