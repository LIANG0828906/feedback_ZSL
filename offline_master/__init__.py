"""Offline turntable mastering renderer.

Deterministic offline pipeline that synthesizes dialogue/footstep/cloth/ambient
stems, aligns transients to picture lip-sync points, corrects intra-group
phase/polarity, spatializes every stem with an analytic binaural model over a
turntable angle sequence, and renders a compliance-checked stereo master.
"""

from .codes import BusinessError, ErrorCode
from .manifest import load_manifest, validate_manifest
from .pipeline import render_project

__all__ = [
    "BusinessError",
    "ErrorCode",
    "load_manifest",
    "validate_manifest",
    "render_project",
]
