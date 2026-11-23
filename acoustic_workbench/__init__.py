"""Multichannel acoustic measurement and calibration workbench."""

from .codes import (
    E_CHAN,
    E_CLIP,
    E_DIFF,
    E_FRAME,
    E_IO,
    E_META,
    E_ORDER,
    E_PARA,
    E_STATE,
    E_TOL,
)
from .errors import WorkbenchError

__all__ = [
    "E_CHAN",
    "E_CLIP",
    "E_DIFF",
    "E_FRAME",
    "E_IO",
    "E_META",
    "E_ORDER",
    "E_PARA",
    "E_STATE",
    "E_TOL",
    "WorkbenchError",
]

__version__ = "1.0.0"
