from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .checkpoint import CheckpointStore
from .engine import EngineResult, process_glyph
from .errors import WorkbenchError
from .flatten import FlatContour
from .geometry import glyph_bbox, refresh_truth
from .model_store import save_document_atomic
from .models import Contour, GlyphDocument, Segment, parse_document
from .params import RenderParams
from .raster import RasterComponent, make_raster_spec


def _round_point(x: float, y: float) -> tuple[int, int]:
    return round(x), round(y)


def _decasteljau(segment: Segment, t: float) -> tuple[Segment, Segment]:
    t = float(t)
    if not (0.0 < t < 1.0):
        raise WorkbenchError("E_PARA", "split parameter must be in (0, 1)")
    if segment.kind == "line":
        p0 = np.asarray(segment.p0, dtype=np.float64)
        p1 = np.asarray(segment.p1, dtype=np.float64)
        split = p0 * (1.0 - t) + p1 * t
        point = _round_point(float(split[0]), float(split[1]))
        return (
            Segment("line", segment.p0, point),
            Segment("line", point, segment.p1),
        )
    if segment.kind == "quadratic":
        p0, p1, p2 = (
            np.asarray(segment.p0, dtype=np.float64),
            np.asarray(segment.p1, dtype=np.float64),
            np.asarray(segment.p2, dtype=np.float64),
        )
        p01 = p0 * (1.0 - t) + p1 * t
        p12 = p1 * (1.0 - t) + p2 * t
        p012 = p01 * (1.0 - t) + p12 * t
        m = _round_point(float(p012[0]), float(p012[1]))
        return (
            Segment(
                "quadratic",
                segment.p0,
                _round_point(float(p01[0]), float(p01[1])),
                m,
            ),
            Segment(
                "quadratic",
                m,
                _round_point(float(p12[0]), float(p12[1])),
                segment.p2,
            ),
        )
    p0, p1, p2, p3 = (
        np.asarray(segment.p0, dtype=np.float64),
        np.asarray(segment.p1, dtype=np.float64),
        np.asarray(segment.p2, dtype=np.float64),
        np.asarray(segment.p3, dtype=np.float64),
    )
    p01 = p0 * (1.0 - t) + p1 * t
    p12 = p1 * (1.0 - t) + p2 * t
    p23 = p2 * (1.0 - t) + p3 * t
    p012 = p01 * (1.0 - t) + p12 * t
    p123 = p12 * (1.0 - t) + p23 * t
    p0123 = p012 * (1.0 - t) + p123 * t
    m = _round_point(float(p0123[0]), float(p0123[1]))
    return (
        Segment(
            "cubic",
            segment.p0,
            _round_point(float(p01[0]), float(p01[1])),
            _round_point(float(p012[0]), float(p012[1])),
            m,
        ),
        Segment(
            "cubic",
            m,
            _round_point(float(p123[0]), float(p123[1])),
            _round_point(float(p23[0]), float(p23[1])),
            segment.p3,
        ),
    )


def _endpoint_name(segment: Segment) -> str:
    if segment.kind == "line":
        return "p1"
    if segment.kind == "quadratic":
        return "p2"
    return "p3"


@dataclass
class CommitInfo:
    success: bool
    affected_contours: list[int]
    reused_contours: list[int]
    result: EngineResult | None
    message: str


@dataclass
class EditSession:
    document: GlyphDocument
    glyph_id: str
    parameters: RenderParams
    persist_root: str | Path
    model_path: str | Path | None = None
    _committed_document: GlyphDocument = field(init=False, repr=False)
    _working_document: GlyphDocument = field(init=False, repr=False)
    _committed_params: RenderParams = field(init=False)
    _working_params: RenderParams = field(init=False)
    _dirty_contours: set[int] = field(default_factory=set)
    _committed_result: EngineResult | None = field(default=None)
    _working_result: EngineResult | None = field(default=None)
    _last_commit: CommitInfo | None = field(default=None)
    _revision: int = 0

    def __post_init__(self) -> None:
        self.document.find_glyph(self.glyph_id)
        self.parameters.validate()
        self._committed_document = self.document.copy()
        self._working_document = self.document.copy()
        self._committed_params = self.parameters
        self._working_params = self.parameters

    @property
    def committed_glyph(self):
        return self._committed_document.find_glyph(self.glyph_id)

    @property
    def working_document(self) -> GlyphDocument:
        return self._working_document

    @property
    def committed_document(self) -> GlyphDocument:
        return self._committed_document

    @property
    def working_glyph(self):
        return self._working_document.find_glyph(self.glyph_id)

    @property
    def committed_parameters(self) -> RenderParams:
        return self._committed_params

    @property
    def working_parameters(self) -> RenderParams:
        return self._working_params

    @property
    def dirty_contours(self) -> set[int]:
        return set(self._dirty_contours)

    @property
    def revision(self) -> int:
        return self._revision

    @property
    def committed_result(self) -> EngineResult | None:
        return self._committed_result

    @property
    def working_result(self) -> EngineResult | None:
        return self._working_result

    @property
    def last_commit(self) -> CommitInfo | None:
        return self._last_commit

    def set_parameters(self, params: RenderParams) -> None:
        params.validate()
        if params == self._working_params:
            return
        geometry_changed = (
            params.pixels_per_unit != self._working_params.pixels_per_unit
            or params.tolerance != self._working_params.tolerance
            or params.grid != self._working_params.grid
        )
        self._working_params = params
        if geometry_changed:
            self._dirty_contours = set(range(len(self.working_glyph.contours)))
        self._revision += 1

    def _assert_contour(self, position: int) -> Contour:
        contours = self.working_glyph.contours
        if position < 0 or position >= len(contours):
            raise WorkbenchError(
                "E_STATE",
                "contour position is outside the working glyph",
                glyph_id=self.glyph_id,
                contour_index=position,
            )
        return contours[position]

    def _check_coordinate(self, point: tuple[int, int]) -> tuple[int, int]:
        glyph = self.working_glyph
        x, y = int(point[0]), int(point[1])
        quantum = glyph.coordinate_quantum
        if not (
            glyph.coordinate_min <= x <= glyph.coordinate_max
            and glyph.coordinate_min <= y <= glyph.coordinate_max
        ):
            raise WorkbenchError(
                "E_PARA",
                "edited control point is outside the glyph coordinate range",
                glyph_id=self.glyph_id,
                point=[x, y],
            )
        if quantum > 0:
            x = int(round(x / quantum) * quantum)
            y = int(round(y / quantum) * quantum)
        return x, y

    def set_control_point(
        self,
        contour_position: int,
        segment_position: int,
        control_name: str,
        point: tuple[int, int],
    ) -> None:
        contour = self._assert_contour(contour_position)
        if segment_position < 0 or segment_position >= len(contour.segments):
            raise WorkbenchError(
                "E_STATE",
                "segment position is outside the working contour",
                glyph_id=self.glyph_id,
                contour_index=contour.index,
                segment_index=segment_position,
            )
        segment = contour.segments[segment_position]
        point = self._check_coordinate(point)
        segment.set_control(control_name, point)
        if control_name == "p0":
            if segment_position == 0:
                contour.start = point
                last = contour.segments[-1]
                setattr(last, _endpoint_name(last), point)
            else:
                previous = contour.segments[segment_position - 1]
                setattr(previous, _endpoint_name(previous), point)
        else:
            endpoint_name = _endpoint_name(segment)
            if control_name == endpoint_name:
                if segment_position + 1 < len(contour.segments):
                    contour.segments[segment_position + 1].p0 = point
                else:
                    contour.start = point
                    contour.segments[0].p0 = point
        self._dirty_contours.add(contour_position)
        refresh_truth(self.working_glyph)
        self._revision += 1

    def split_segment(
        self, contour_position: int, segment_position: int, t: float = 0.5
    ) -> tuple[Segment, Segment]:
        contour = self._assert_contour(contour_position)
        if segment_position < 0 or segment_position >= len(contour.segments):
            raise WorkbenchError(
                "E_STATE",
                "segment position is outside the working contour",
                glyph_id=self.glyph_id,
                contour_index=contour.index,
                segment_index=segment_position,
            )
        original = contour.segments[segment_position]
        left, right = _decasteljau(original, t)
        contour.segments[segment_position : segment_position + 1] = [left, right]
        if segment_position + 2 < len(contour.segments):
            contour.segments[segment_position + 2].p0 = right.endpoint
        else:
            contour.segments[0].p0 = right.endpoint
            contour.start = right.endpoint
        self._dirty_contours.add(contour_position)
        refresh_truth(self.working_glyph)
        self._revision += 1
        return left, right

    def add_segment(
        self, contour_position: int, segment_position: int, t: float = 0.5
    ) -> tuple[Segment, Segment]:
        return self.split_segment(contour_position, segment_position, t=t)

    def delete_segment(self, contour_position: int, segment_position: int) -> None:
        contour = self._assert_contour(contour_position)
        if len(contour.segments) <= 2:
            raise WorkbenchError(
                "E_STATE",
                "a closed contour must retain at least two segments",
                glyph_id=self.glyph_id,
                contour_index=contour.index,
            )
        if segment_position < 0 or segment_position >= len(contour.segments):
            raise WorkbenchError(
                "E_STATE",
                "segment position is outside the working contour",
                glyph_id=self.glyph_id,
                contour_index=contour.index,
                segment_index=segment_position,
            )
        contour.segments.pop(segment_position)
        if segment_position > 0:
            previous = contour.segments[segment_position - 1]
            if segment_position < len(contour.segments):
                contour.segments[segment_position].p0 = previous.endpoint
            else:
                contour.start = previous.endpoint
                contour.segments[0].p0 = contour.start
        else:
            previous = contour.segments[-1]
            contour.start = previous.endpoint
            contour.segments[0].p0 = contour.start
        self._dirty_contours.add(contour_position)
        refresh_truth(self.working_glyph)
        self._revision += 1

    def import_working_result(self, result: EngineResult) -> None:
        self._working_result = result

    def process_working(
        self,
        *,
        force_full: bool = False,
        pause_event: Any = None,
        on_commit: Any = None,
        on_progress: Any = None,
    ) -> EngineResult:
        result = process_glyph(
            self.working_glyph,
            self._working_params,
            persist_root=self.persist_root,
            force_full=force_full,
            pause_event=pause_event,
            on_commit=on_commit,
            on_progress=on_progress,
            preserve_other_snapshots=True,
        )
        self._working_result = result
        return result

    def _reusable_components(
        self,
        candidate_glyph,
        candidate_params: RenderParams,
    ) -> dict[int, tuple[FlatContour, RasterComponent]]:
        old_result = self._committed_result
        if old_result is None or not old_result.complete:
            return {}
        if candidate_params != self._committed_params:
            return {}
        if glyph_bbox(candidate_glyph) != glyph_bbox(self.committed_glyph):
            return {}
        old_spec = make_raster_spec(self.committed_glyph, self._committed_params)
        new_spec = make_raster_spec(candidate_glyph, candidate_params)
        if old_spec != new_spec:
            return {}
        if not old_result.flat_contours or len(old_result.flat_contours) != len(
            candidate_glyph.contours
        ):
            return {}
        components_by_contour = {
            component.contour_index: component for component in old_result.raster.components
        }
        reusable: dict[int, tuple[FlatContour, RasterComponent]] = {}
        for position, contour in enumerate(candidate_glyph.contours):
            if position in self._dirty_contours:
                continue
            component = components_by_contour.get(contour.index)
            flat = old_result.flat_contours[position]
            if component is not None and flat is not None:
                reusable[position] = (flat, component)
        return reusable

    def commit(self) -> CommitInfo:
        candidate_document = self._working_document.copy()
        candidate_glyph = candidate_document.find_glyph(self.glyph_id)
        refresh_truth(candidate_glyph)
        parse_document(candidate_document.to_dict(include_truth=True))
        affected = sorted(self._dirty_contours) or list(
            range(len(candidate_glyph.contours))
        )
        reusable = self._reusable_components(
            candidate_glyph, self._working_params
        )
        try:
            result = process_glyph(
                candidate_glyph,
                self._working_params,
                persist_root=self.persist_root,
                reusable=reusable,
                preserve_other_snapshots=True,
            )
            if not result.complete:
                raise WorkbenchError(
                    "E_STATE",
                    "batch commit processing did not commit every segment",
                    glyph_id=self.glyph_id,
                    failures=result.failures,
                )
            if self.model_path is not None:
                save_document_atomic(candidate_document, self.model_path)
        except Exception as exc:
            self.rollback()
            if isinstance(exc, WorkbenchError):
                raise
            raise WorkbenchError(
                "E_IO",
                f"batch commit failed and was rolled back: {exc}",
                glyph_id=self.glyph_id,
            ) from exc

        try:
            CheckpointStore(
                self.persist_root, candidate_glyph, self._working_params
            ).prune_other_snapshots()
        except Exception:  # noqa: BLE001, S110 - post-commit housekeeping.
            # Pruning historical snapshots is housekeeping; it cannot undo a
            # model commit that has already been atomically persisted.
            pass

        reused_positions = sorted(reusable)
        self._committed_document = candidate_document.copy()
        self._working_document = candidate_document.copy()
        self._committed_params = self._working_params
        self._committed_result = result
        self._working_result = result
        self._dirty_contours.clear()
        self._revision += 1
        self._last_commit = CommitInfo(
            success=True,
            affected_contours=affected,
            reused_contours=reused_positions,
            result=result,
            message="batch committed",
        )
        return self._last_commit

    def rollback(self) -> None:
        self._working_document = self._committed_document.copy()
        self._working_params = self._committed_params
        self._working_result = self._committed_result
        self._dirty_contours.clear()
        self._revision += 1

    def export_result(self) -> EngineResult:
        if self._committed_result is not None:
            return self._committed_result
        result = process_glyph(
            self.committed_glyph,
            self._committed_params,
            persist_root=self.persist_root,
            preserve_other_snapshots=True,
        )
        if not result.complete:
            raise WorkbenchError(
                "E_STATE",
                "committed export result is incomplete",
                glyph_id=self.glyph_id,
            )
        self._committed_result = result
        return result

