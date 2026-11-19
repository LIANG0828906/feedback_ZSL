"""Local telemetry settlement executor."""

from .errors import SettlementError

__all__ = ["Executor", "ExecutionConfig", "ExecutionResult", "SettlementError"]
__version__ = "0.1.0"


def __getattr__(name: str):
    if name in {"Executor", "ExecutionConfig", "ExecutionResult"}:
        from . import engine

        return getattr(engine, name)
    raise AttributeError(name)

