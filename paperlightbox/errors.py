"""Structured business error codes.

Every validation failure is reported with a stable integer code, the layer
number (when applicable) and a human readable reason.  The render entry point
writes the payload as JSON and exits with a non-zero integer status, without
emitting any partial image.
"""

from __future__ import annotations

import json
from dataclasses import dataclass


ERROR_CODES = {
    1: "SCENE_JSON_INVALID",
    2: "MESH_INDEX_OUT_OF_RANGE",
    3: "DEGENERATE_TRIANGLE",
    4: "TRANSMITTANCE_OUT_OF_RANGE",
    5: "LAYER_INDEX_DUPLICATE_OR_MISSING",
    6: "LIGHT_INSIDE_LAYER_GEOMETRY",
    7: "INVALID_CAMERA",
    8: "INVALID_LIGHT",
    9: "INVALID_TILE_OR_SAMPLING",
    10: "CUTOUT_MASK_MISMATCH",
}


@dataclass
class SceneError(Exception):
    code: int
    reason: str
    layer: int | None = None

    def __post_init__(self) -> None:
        super().__init__(self.reason)

    def to_payload(self) -> dict:
        payload = {
            "status": "error",
            "error_code": self.code,
            "error_name": ERROR_CODES.get(self.code, "UNKNOWN_ERROR"),
            "reason": self.reason,
        }
        if self.layer is not None:
            payload["layer"] = self.layer
        return payload


def render_error_payload(error: SceneError) -> str:
    """Stable JSON serialization (sorted, fixed separators, no timestamps)."""

    return json.dumps(error.to_payload(), sort_keys=True, separators=(",", ":"))
