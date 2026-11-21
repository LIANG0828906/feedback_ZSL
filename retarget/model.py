"""Unified in-memory model shared by all animation format versions."""

from dataclasses import dataclass, field
from typing import Dict, List, Optional

import numpy as np


@dataclass
class Animation:
    """Version-upgraded animation. v1/v2 inputs are upgraded to v3 semantics
    with documented defaults:
      - v1: flat hierarchy (every joint parented to the virtual root),
            no translations, identity bind pose, unit scales, no aliases.
      - v2: identity bind pose, unit scales, no aliases.
    """

    name: str
    version: int
    joints: List[str]
    hierarchy: Dict[str, Optional[str]]          # joint -> parent or None
    rotations: Dict[str, np.ndarray]             # joint -> (F, 4) raw quats
    translations: Optional[Dict[str, np.ndarray]]  # joint -> (F, 3) or None
    bind_global: Dict[str, np.ndarray]           # joint -> (4, 4)
    bone_scales: Dict[str, float]
    aliases: Dict[str, str]
    frame_count: int
    quat_norm_tolerance: float
    has_translations: bool
    has_bind_pose: bool


@dataclass
class Skeleton:
    """Target skeleton description."""

    name: str
    joints: List[str]
    hierarchy: Dict[str, Optional[str]]
    bind_global: Dict[str, np.ndarray]           # joint -> (4, 4)
    bone_lengths: Dict[str, float] = field(default_factory=dict)
