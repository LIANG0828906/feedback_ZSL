"""Deterministic HDR atlas conversion pipeline."""

from .codec import decode_atlas, decode_atlas_bytes, encode_atlas_bytes, read_atlas
from .color import ColorChain, build_rgb_to_rgb_matrix
from .errors import AtlasError, ErrorCode
from .export import ExportResult, export_published, verify_export
from .model import (
    AtlasHeader,
    AtlasImage,
    TileErrorSummary,
    TileParams,
    TileSpec,
    TransformConfig,
    Version,
)
from .pipeline import AtlasPipeline, ConversionJob, VersionDiff
from .render import render_preview_png
from .sample import generate_atlas, generate_suite

__all__ = [
    "AtlasError",
    "AtlasHeader",
    "AtlasImage",
    "AtlasPipeline",
    "ColorChain",
    "ConversionJob",
    "ErrorCode",
    "ExportResult",
    "TileErrorSummary",
    "TileParams",
    "TileSpec",
    "TransformConfig",
    "Version",
    "VersionDiff",
    "build_rgb_to_rgb_matrix",
    "decode_atlas",
    "decode_atlas_bytes",
    "encode_atlas_bytes",
    "export_published",
    "generate_atlas",
    "generate_suite",
    "read_atlas",
    "render_preview_png",
    "verify_export",
]

__version__ = "1.0.0"
