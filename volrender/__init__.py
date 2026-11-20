"""Deterministic desktop volume rendering component."""

from .camera import default_view_matrix, look_at_view
from .errors import (
    E_BUSY,
    E_FORMAT,
    E_INPUT_INVALID,
    E_IO,
    E_LIGHT_DRIFT,
    E_MODE_INVALID,
    E_SNAPSHOT_INVALID,
    E_SNAPSHOT_STALE,
    E_TF_INVALID,
    VolRenderError,
)
from .format import read_volume_file, write_volume_file
from .render import (
    LightDriftReport,
    ModeSwitchReport,
    PrecisionComparison,
    RenderResult,
    VolumeRenderer,
)
from .session import RenderSession
from .snapshot import Snapshot, load_snapshot, parse_snapshot, save_snapshot
from .transfer import ControlPoint, LUTBundle, TransferFunction
from .volume import MappingChange, NormalizationReference, VolumeData

__all__ = [
    "ControlPoint",
    "E_BUSY",
    "E_FORMAT",
    "E_INPUT_INVALID",
    "E_IO",
    "E_LIGHT_DRIFT",
    "E_MODE_INVALID",
    "E_SNAPSHOT_INVALID",
    "E_SNAPSHOT_STALE",
    "E_TF_INVALID",
    "LightDriftReport",
    "LUTBundle",
    "MappingChange",
    "ModeSwitchReport",
    "NormalizationReference",
    "PrecisionComparison",
    "RenderResult",
    "RenderSession",
    "Snapshot",
    "TransferFunction",
    "VolRenderError",
    "VolumeData",
    "VolumeRenderer",
    "default_view_matrix",
    "load_snapshot",
    "look_at_view",
    "parse_snapshot",
    "read_volume_file",
    "save_snapshot",
    "write_volume_file",
]
