"""Content-addressed local volume rendering toolkit."""

from .buffers import BufferStore, StoredBuffer
from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters, default_view_matrix, make_parameters
from .preview import PreviewController, PreviewFrame, validate_preview_binding
from .renderer import (
    CancellationToken,
    RenderCancelled,
    RenderResult,
    render_offscreen,
)
from .snapshot import (
    RestoreOutcome,
    Snapshot,
    SnapshotManager,
    create_snapshot,
    load_snapshot,
    recover_from_snapshot,
    save_snapshot,
)
from .synthetic import baseline_parameters, generate_sample, generate_samples, make_synthetic_volume
from .tasks import CancellationFlag, OffscreenRenderService, RenderJob, wait_for_job
from .trace import DeterministicTrace, read_trace
from .transfer import (
    ControlPoint,
    DEFAULT_CONTROL_POINTS,
    TransferFunction,
    build_transfer_lut,
    validate_control_points,
)
from .volume import (
    FORMAT_VERSION,
    HEADER_SIZE,
    VolumeData,
    VolumeHeader,
    WindowDelta,
    WindowReference,
    compare_window_mappings,
    load_volume,
    map_window,
    parse_header,
    write_volume,
)

__all__ = [
    "BufferStore",
    "CancellationFlag",
    "CancellationToken",
    "ControlPoint",
    "DEFAULT_CONTROL_POINTS",
    "DeterministicTrace",
    "ErrorCode",
    "FORMAT_VERSION",
    "HEADER_SIZE",
    "OffscreenRenderService",
    "PreviewController",
    "PreviewFrame",
    "RenderCancelled",
    "RenderJob",
    "RenderParameters",
    "RenderResult",
    "RestoreOutcome",
    "Snapshot",
    "SnapshotManager",
    "StoredBuffer",
    "TransferFunction",
    "VolumeData",
    "VolumeHeader",
    "VolumeRenderError",
    "WindowDelta",
    "WindowReference",
    "baseline_parameters",
    "build_transfer_lut",
    "compare_window_mappings",
    "create_snapshot",
    "default_view_matrix",
    "generate_sample",
    "generate_samples",
    "load_snapshot",
    "load_volume",
    "make_parameters",
    "make_synthetic_volume",
    "map_window",
    "parse_header",
    "read_trace",
    "recover_from_snapshot",
    "render_offscreen",
    "save_snapshot",
    "validate_control_points",
    "validate_preview_binding",
    "wait_for_job",
    "write_volume",
]
