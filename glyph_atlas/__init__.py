"""Local deterministic glyph rasterizer and atlas workbench."""

from .atlas import AtlasBuildResult, build_atlas, verify_output_hashes
from .config import BudgetConfig, AtlasConfig, TargetConfig, WorkConfig, load_config
from .errors import (
    E_ATLAS_OVERFLOW,
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    E_CONFIG_INVALID,
    E_GLYPH_INVALID,
    E_SNAPSHOT_MISMATCH,
    E_TILE_MISMATCH,
    GlyphAtlasError,
)
from .font import FontSpec, ResolvedGlyph, load_font, parse_font
from .pipeline import PipelineReport, run_pipeline
from .raster import (
    GlyphRenderPlan,
    PixelBox,
    SamplingSpec,
    compare_coverage,
    make_render_plan,
    rasterize_tile,
    rasterize_whole,
)
from .scheduler import GlyphRasterResult, RasterScheduler, SchedulerReport
from .snapshot import SNAPSHOT_VERSION, SnapshotStore

__all__ = [
    "AtlasBuildResult",
    "AtlasConfig",
    "BudgetConfig",
    "E_ATLAS_OVERFLOW",
    "E_BUDGET_EXCEEDED",
    "E_BUSY",
    "E_CANCELLED",
    "E_CONFIG_INVALID",
    "E_GLYPH_INVALID",
    "E_SNAPSHOT_MISMATCH",
    "E_TILE_MISMATCH",
    "FontSpec",
    "GlyphAtlasError",
    "GlyphRasterResult",
    "GlyphRenderPlan",
    "PipelineReport",
    "PixelBox",
    "RasterScheduler",
    "ResolvedGlyph",
    "SNAPSHOT_VERSION",
    "SamplingSpec",
    "SchedulerReport",
    "SnapshotStore",
    "TargetConfig",
    "WorkConfig",
    "build_atlas",
    "compare_coverage",
    "load_config",
    "load_font",
    "make_render_plan",
    "parse_font",
    "rasterize_tile",
    "rasterize_whole",
    "run_pipeline",
    "verify_output_hashes",
]

