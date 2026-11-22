"""Incremental re-recognition scheduling and full-run consistency check.

Adjusting the merge threshold or confidence floor invalidates only
uncommitted intervals (and their downstream reading-order indices); invalid
intervals are recomputed in interval order while valid intervals reuse the
persistent cache. Committed intervals keep their frozen parameter snapshot.
A full recompute from an empty cache uses the same effective per-interval
parameters, so the two paths must agree line by line; the first mismatch is
reported as E_DIFF with image id, line id and field name.
"""

from __future__ import annotations

from typing import Dict, List, Optional, Sequence, Tuple

from . import engine
from .errors import DiffError
from .model import TextLine
from .state import IntervalState, build_intervals

Params = Tuple[float, float]


class IncrementalSession:
    def __init__(
        self,
        image_id: str,
        regions: Sequence[engine.GtRegion],
        *,
        lines_per_interval: int = 2,
        merge_threshold: float = engine.DEFAULT_MERGE_THRESHOLD,
        conf_threshold: float = engine.DEFAULT_CONF_THRESHOLD,
    ) -> None:
        self.image_id = image_id
        self.regions = list(regions)
        self.params: Params = (merge_threshold, conf_threshold)
        probe = engine.detect_and_recognize(
            image_id,
            self.regions,
            merge_threshold=merge_threshold,
            conf_threshold=conf_threshold,
        )
        self.intervals = build_intervals(image_id, len(probe), lines_per_interval)
        self._cache: Dict[int, Tuple[TextLine, ...]] = {}
        self._valid: Dict[int, bool] = {
            iv.interval_index: False for iv in self.intervals
        }
        self._frozen: Dict[int, Params] = {}

    # -- interval computation ------------------------------------------------

    def _compute_interval(self, interval_index: int, params: Params):
        merge_threshold, conf_threshold = params
        lines = engine.detect_and_recognize(
            self.image_id,
            self.regions,
            merge_threshold=merge_threshold,
            conf_threshold=conf_threshold,
        )
        iv = self.intervals[interval_index]
        return tuple(lines[iv.line_start:iv.line_end])

    def recognize_all(self) -> None:
        """Recognize every invalid interval in interval order.

        Computation fills the cache only; interval states advance solely
        through the legal state-machine transitions at commit time.
        """
        for iv in self.intervals:
            idx = iv.interval_index
            if iv.state is IntervalState.COMMITTED:
                continue
            if not self._valid.get(idx, False):
                self._cache[idx] = self._compute_interval(idx, self.params)
                self._valid[idx] = True

    def commit_interval(self, interval_index: int) -> None:
        iv = self.intervals[interval_index]
        if not self._valid.get(interval_index, False):
            self._cache[interval_index] = self._compute_interval(
                interval_index, self.params
            )
            self._valid[interval_index] = True
        iv.transition(IntervalState.RECOGNIZING)
        iv.commit(*self.params, source_version="session")
        self._frozen[interval_index] = self.params

    # -- parameter adjustment --------------------------------------------------

    def adjust_params(self, merge_threshold: float, conf_threshold: float) -> List[int]:
        """Invalidate uncommitted intervals; return invalidated indices."""
        self.params = (merge_threshold, conf_threshold)
        invalidated = []
        for iv in self.intervals:
            if iv.state is IntervalState.COMMITTED:
                continue  # frozen snapshot: adjustments never touch it
            self._valid[iv.interval_index] = False
            invalidated.append(iv.interval_index)
        return invalidated

    # -- views -----------------------------------------------------------------

    def effective_params(self, interval_index: int) -> Params:
        if interval_index in self._frozen:
            return self._frozen[interval_index]
        return self.params

    def lines(self) -> Tuple[TextLine, ...]:
        self.recognize_all()
        out: List[TextLine] = []
        for iv in self.intervals:
            out.extend(self._cache[iv.interval_index])
        return tuple(out)

    def full_recompute(self) -> Tuple[TextLine, ...]:
        """From-empty-cache recompute using effective per-interval params."""
        out: List[TextLine] = []
        for iv in self.intervals:
            out.extend(
                self._compute_interval(
                    iv.interval_index, self.effective_params(iv.interval_index)
                )
            )
        return tuple(out)

    # -- consistency check -----------------------------------------------------

    def compare_with_full(self) -> None:
        incremental = self.lines()
        full = self.full_recompute()
        if len(incremental) != len(full):
            raise DiffError(
                "line count differs between incremental and full recognition",
                image_id=self.image_id,
                line_id=-1,
                field="line_count",
            )
        for inc, ful in zip(incremental, full):
            for field_name in ("text", "char_conf", "order_index"):
                if getattr(inc, field_name) != getattr(ful, field_name):
                    raise DiffError(
                        "incremental vs full recognition mismatch",
                        image_id=self.image_id,
                        line_id=inc.line_id,
                        field=field_name,
                    )
