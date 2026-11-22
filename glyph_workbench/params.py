from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, dataclass
from typing import Any

from .errors import WorkbenchError

ALGORITHM_VERSION = "glyph-raster/1:adaptive-bezier+scanline-supersample"
FLATTEN_ALGORITHM = "adaptive-decasteljau/1"
RASTER_ALGORITHM = "evenodd-scanline-grid-center/1"


@dataclass(frozen=True)
class RenderParams:
    pixels_per_unit: float = 0.25
    tolerance: float = 0.35
    grid: int = 8
    max_workers: int = 2
    max_buffer_bytes: int = 64 * 1024 * 1024

    def validate(self) -> None:
        if not math.isfinite(self.pixels_per_unit) or not (
            0.01 <= self.pixels_per_unit <= 64.0
        ):
            raise WorkbenchError(
                "E_PARA",
                "pixels_per_unit must be finite and in [0.01, 64.0]",
                context={"pixels_per_unit": self.pixels_per_unit},
            )
        if not math.isfinite(self.tolerance) or not (
            1e-4 <= self.tolerance <= 16.0
        ):
            raise WorkbenchError(
                "E_PARA",
                "flattening tolerance must be finite and in [1e-4, 16.0]",
                context={"tolerance": self.tolerance},
            )
        if isinstance(self.grid, bool) or not isinstance(self.grid, int):
            raise WorkbenchError("E_PARA", "raster grid must be an integer")
        if not (1 <= self.grid <= 16):
            raise WorkbenchError(
                "E_PARA",
                "raster grid must be in [1, 16]",
                context={"grid": self.grid},
            )
        if isinstance(self.max_workers, bool) or not isinstance(self.max_workers, int):
            raise WorkbenchError("E_PARA", "max_workers must be an integer")
        if not (1 <= self.max_workers <= 16):
            raise WorkbenchError(
                "E_PARA",
                "max_workers must be in [1, 16]",
                context={"max_workers": self.max_workers},
            )
        if self.max_buffer_bytes < 1024:
            raise WorkbenchError(
                "E_PARA",
                "max_buffer_bytes must be at least 1024",
                context={"max_buffer_bytes": self.max_buffer_bytes},
            )

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)

    def params_hash(self) -> str:
        self.validate()
        payload = {
            "pixels_per_unit": round(self.pixels_per_unit, 12),
            "tolerance": round(self.tolerance, 12),
            "grid": self.grid,
            "flatten": FLATTEN_ALGORITHM,
            "raster": RASTER_ALGORITHM,
        }
        text = json.dumps(payload, sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(text.encode("utf-8")).hexdigest()

    def snapshot_key(self) -> str:
        return self.params_hash()[:20]

