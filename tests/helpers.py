from __future__ import annotations

from vpipe.io import parse_path
from vpipe.model import PathData


def rectangle(
    x0: float,
    y0: float,
    x1: float,
    y1: float,
    contour_id: str = "rect",
) -> PathData:
    return parse_path(
        {
            "version": 1,
            "fill_rule": "nonzero",
            "contours": [
                {
                    "id": contour_id,
                    "start": [x0, y0],
                    "segments": [
                        {"type": "line", "to": [x1, y0]},
                        {"type": "line", "to": [x1, y1]},
                        {"type": "line", "to": [x0, y1]},
                        {"type": "line", "to": [x0, y0]},
                    ],
                }
            ],
        }
    )


def holed_square() -> PathData:
    return parse_path(
        {
            "version": 1,
            "fill_rule": "nonzero",
            "contours": [
                {
                    "id": "outer",
                    "start": [0, 0],
                    "segments": [
                        {"type": "line", "to": [30, 0]},
                        {"type": "line", "to": [30, 30]},
                        {"type": "line", "to": [0, 30]},
                        {"type": "line", "to": [0, 0]},
                    ],
                },
                {
                    "id": "hole",
                    "start": [10, 10],
                    "segments": [
                        {"type": "line", "to": [20, 10]},
                        {"type": "line", "to": [20, 20]},
                        {"type": "line", "to": [10, 20]},
                        {"type": "line", "to": [10, 10]},
                    ],
                },
            ],
        }
    )

