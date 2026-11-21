from __future__ import annotations

from typing import Any


def rectangle(
    x0: float, y0: float, x1: float, y1: float, *, clockwise: bool = False
) -> dict[str, Any]:
    points = (
        [(x0, y0), (x0, y1), (x1, y1), (x1, y0)]
        if clockwise
        else [(x0, y0), (x1, y0), (x1, y1), (x0, y1)]
    )
    return {
        "fill_rule": "nonzero",
        "subpaths": [
            {
                "closed": True,
                "segments": [
                    {
                        "kind": "L",
                        "points": [
                            list(points[index]),
                            list(points[(index + 1) % len(points)]),
                        ],
                    }
                    for index in range(len(points))
                ],
            }
        ],
    }


def ring_with_hole() -> dict[str, Any]:
    outer = rectangle(0.0, 0.0, 4.0, 4.0)["subpaths"][0]
    hole = rectangle(1.0, 1.0, 3.0, 3.0, clockwise=True)["subpaths"][0]
    return {"fill_rule": "nonzero", "subpaths": [outer, hole]}

