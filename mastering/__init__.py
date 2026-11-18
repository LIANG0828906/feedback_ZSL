"""Deterministic offline multilingual mastering."""

__all__ = ["MasteringEngine"]
__version__ = "1.0.0"


def __getattr__(name: str):
    if name == "MasteringEngine":
        from .engine import MasteringEngine

        return MasteringEngine
    raise AttributeError(name)

