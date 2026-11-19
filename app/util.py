from __future__ import annotations

import math
import time
from datetime import datetime, timezone
from typing import Iterable


def now_ms() -> int:
    return int(time.time() * 1000)


def iso_utc(ms: int | None) -> str | None:
    if ms is None:
        return None
    return datetime.fromtimestamp(ms / 1000.0, tz=timezone.utc).isoformat(
        timespec="milliseconds"
    ).replace("+00:00", "Z")


def percentile(values: Iterable[int | float], fraction: float) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    index = max(0, math.ceil(fraction * len(ordered)) - 1)
    return float(ordered[min(index, len(ordered) - 1)])


def rounded(value: float, places: int = 3) -> float:
    return round(float(value), places)

