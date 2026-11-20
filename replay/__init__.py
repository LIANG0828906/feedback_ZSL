"""确定性回合制对局回放系统。"""

from .config import REPLAYER_VERSION, FrozenConfig, load_config, write_default_config
from .engine import RuleEngine
from .errors import (
    E_BASELINE_MISMATCH,
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    E_RECOVERY_INVALID,
    OK,
)
from .model import MatchDescription, TurnBudget
from .scheduler import ReplayScheduler
from .verifier import VerificationReport, generate_baseline, verify

__all__ = [
    "REPLAYER_VERSION",
    "FrozenConfig",
    "load_config",
    "write_default_config",
    "RuleEngine",
    "MatchDescription",
    "TurnBudget",
    "ReplayScheduler",
    "VerificationReport",
    "generate_baseline",
    "verify",
    "OK",
    "E_BUSY",
    "E_BUDGET_EXCEEDED",
    "E_RECOVERY_INVALID",
    "E_BASELINE_MISMATCH",
    "E_CANCELLED",
]
