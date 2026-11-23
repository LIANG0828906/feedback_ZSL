"""Ground-truth re-projection validation across the transform chain."""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .errors import GeoError, ErrorCode
from .stages import StageConfig


@dataclass
class StageErrorReport:
    stage_id: str
    median: float
    upper: float
    failures: list = field(default_factory=list)


@dataclass
class GeometryReport:
    tolerance: float
    stages: list
    ok: bool

    def summary(self) -> str:
        lines = [f"tolerance={self.tolerance:.4g}px ok={self.ok}"]
        for rep in self.stages:
            lines.append(
                f"  {rep.stage_id}: median={rep.median:.4g} "
                f"upper={rep.upper:.4g} failures={len(rep.failures)}")
        return "\n".join(lines)


def validate_geometry(stages: list, truth: dict, tolerance: float = 0.5
                      ) -> GeometryReport:
    """Forward-project source control points and compare per stage.

    truth structure (produced by the deterministic sample generator)::

        {
          "points_source":   Nx2 source-frame coordinates,
          "points_by_stage": {stage_id: Nx2 expected coordinates},
          "distortion":      {"center": [cx, cy], "radius": r},
        }
    """
    points = np.asarray(truth["points_source"], dtype=np.float64)
    expected_by_stage = truth["points_by_stage"]
    reports = []
    overall_ok = True
    current = points
    for stage in stages:
        current = stage.forward_points(current)
        expected = np.asarray(expected_by_stage[stage.stage_id],
                              dtype=np.float64)
        errors = np.linalg.norm(current - expected, axis=1)
        bad_idx = np.flatnonzero(errors > tolerance)
        failures = [{
            "id": int(idx),
            "measured": [float(current[idx, 0]), float(current[idx, 1])],
            "expected": [float(expected[idx, 0]), float(expected[idx, 1])],
            "error": float(errors[idx]),
        } for idx in bad_idx]
        report = StageErrorReport(
            stage_id=stage.stage_id,
            median=float(np.median(errors)),
            upper=float(np.max(errors)),
            failures=failures,
        )
        if failures:
            overall_ok = False
        reports.append(report)
    if not overall_ok:
        worst = max((f for rep in reports for f in rep.failures),
                    key=lambda f: f["error"])
        raise GeoError(
            ErrorCode.E_GEOM,
            "re-projection error exceeds tolerance; "
            f"worst control point #{worst['id']} "
            f"measured={worst['measured']} error={worst['error']:.4g}px",
            detail={"tolerance": tolerance,
                    "failures": [f for rep in reports for f in rep.failures]})
    return GeometryReport(tolerance=tolerance, stages=reports, ok=True)
