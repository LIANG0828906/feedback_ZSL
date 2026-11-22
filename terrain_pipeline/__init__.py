"""Deterministic terrain render-data bake pipeline.

The public API is intentionally small:

* :func:`terrain_pipeline.loader.load_project` validates and loads a project.
* :class:`terrain_pipeline.pipeline.Pipeline` performs full, incremental, and
  recovery bakes.
* :func:`terrain_pipeline.compare.compare_snapshots` provides byte-level
  parity diagnostics.
"""

from .errors import PipelineError
from .loader import load_project
from .model import BakeParams, TerrainProject
from .pipeline import BakeResult, Pipeline

__all__ = [
    "BakeParams",
    "BakeResult",
    "Pipeline",
    "PipelineError",
    "TerrainProject",
    "load_project",
]
