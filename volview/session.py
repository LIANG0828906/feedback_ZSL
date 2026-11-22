"""Reading session: interactive state, rendering, annotations, snapshots.

A session binds a :class:`VolumeModel` to viewing parameters (window,
plane, plane position, thickness, interpolation order) and a list of
annotations.  Every mutating operation followed by :meth:`commit` persists
a full snapshot (parameters + annotations + the current plane image) so a
later session restores the identical view.

Rendering honours the scheduler's per-task pixel buffer limit by computing
the plane in row chunks when the plane exceeds the limit; chunking is a
pure implementation detail and yields pixel-identical results.
"""

from __future__ import annotations

from typing import List, Optional, Sequence

import numpy as np

from .errors import ErrorCode, VolViewError
from .measure import (Annotation, accuracy_bound, make_annotation,
                      project_to_plane)
from .model import VolumeModel, VolumeRegistry
from .resample import (PLANES, render_plane, validate_interp_order,
                       validate_plane_position, validate_window, plane_shape)
from .slab import render_slab, validate_thickness
from .snapshot import SNAPSHOT_SCHEMA_VERSION, SnapshotStore

DEFAULT_WINDOW_WIDTH = 1500.0
DEFAULT_WINDOW_LEVEL = 300.0


def _clamp_window_to_range(model: VolumeModel, ww: float, wl: float):
    lo, hi = model.manifest.gray_range
    wl = min(max(wl, lo), hi)
    ww = min(ww, hi - lo)
    return ww, wl


class ReadingSession:
    def __init__(self, model: VolumeModel, store: SnapshotStore,
                 session_id: str, interp_order: int = 1):
        self.model = model
        self.store = store
        self.session_id = session_id
        self.interp_order = validate_interp_order(interp_order)
        ww, wl = _clamp_window_to_range(model, DEFAULT_WINDOW_WIDTH,
                                        DEFAULT_WINDOW_LEVEL)
        self.window_width = ww
        self.window_level = wl
        self.plane = "axial"
        self.plane_position = model.extent("Z") / 2.0
        self.thickness = 0.0
        self.annotations: List[Annotation] = []
        self._next_annotation_id = 0
        self._restored = False

    # -- construction -------------------------------------------------------
    @classmethod
    def open(cls, registry: VolumeRegistry, volume_dir: str,
             store: SnapshotStore, session_id: str,
             interp_order: Optional[int] = 1) -> "ReadingSession":
        """Open a session; restore the snapshot when fully valid."""
        model = registry.get(volume_dir)
        requested_order = interp_order
        snapshot = store.load(session_id, model.content_hash, requested_order)
        order = requested_order if requested_order is not None else 1
        session = cls(model, store, session_id, order)
        if snapshot is not None:
            params = snapshot["params"]
            session.window_width = float(params["window_width"])
            session.window_level = float(params["window_level"])
            session.plane = params["plane"]
            session.plane_position = float(params["plane_position"])
            session.thickness = float(params["thickness"])
            session.interp_order = int(params["interp_order"])
            session.annotations = [
                Annotation(
                    id=a["id"], plane=a["plane"],
                    plane_position=float(a.get("plane_position", 0.0)),
                    endpoints_world=[list(map(float, p)) for p in a["endpoints_world"]],
                    endpoints_voxel=[list(map(float, p)) for p in a["endpoints_voxel"]],
                    world_length=float(a["world_length"]),
                    voxel_length=float(a["voxel_length"]),
                    true_length=a.get("true_length"),
                )
                for a in snapshot.get("annotations", [])
            ]
            session._next_annotation_id = (
                max((a.id for a in session.annotations), default=-1) + 1)
            session._restored = True
        return session

    @property
    def restored(self) -> bool:
        return self._restored

    # -- parameter setters (validated) ---------------------------------------
    def set_window(self, window_width: float, window_level: float) -> None:
        self.window_width, self.window_level = validate_window(
            self.model, window_width, window_level)

    def set_plane(self, plane: str, position: Optional[float] = None) -> None:
        if plane not in PLANES:
            raise VolViewError(ErrorCode.E_RANGE, "plane",
                               f"unknown plane {plane!r}")
        self.plane = plane
        if position is None:
            from .resample import PLANE_NORMAL
            position = self.model.extent(PLANE_NORMAL[plane]) / 2.0
        self.plane_position = validate_plane_position(self.model, plane, position)

    def set_plane_position(self, position: float) -> None:
        self.plane_position = validate_plane_position(
            self.model, self.plane, position)

    def set_thickness(self, thickness: float) -> None:
        self.thickness = validate_thickness(self.model, self.plane, thickness)

    def set_interp_order(self, order: int) -> None:
        self.interp_order = validate_interp_order(order)

    # -- rendering -------------------------------------------------------------
    def render(self, max_task_pixels: Optional[int] = None) -> np.ndarray:
        """Current plane as a display-gray (uint8) image.

        When ``max_task_pixels`` is given and the plane exceeds it, the image
        is computed in row chunks (per-thread buffer limit); the result is
        identical to the unchunked computation.
        """
        rows, cols = plane_shape(self.model, self.plane)
        if max_task_pixels is None or rows * cols <= max_task_pixels:
            return self._render_full()
        chunk_rows = max(1, max_task_pixels // cols)
        out = np.empty((rows, cols), dtype=np.uint8)
        for start in range(0, rows, chunk_rows):
            stop = min(rows, start + chunk_rows)
            out[start:stop] = self._render_rows(start, stop)
        return out

    def _render_full(self) -> np.ndarray:
        if self.thickness == 0.0:
            return render_plane(self.model, self.plane, self.plane_position,
                                self.window_width, self.window_level,
                                self.interp_order)
        return render_slab(self.model, self.plane, self.plane_position,
                           self.thickness, self.window_width,
                           self.window_level, self.interp_order)

    def _render_rows(self, row_start: int, row_stop: int) -> np.ndarray:
        row_range = (row_start, row_stop)
        if self.thickness == 0.0:
            return render_plane(self.model, self.plane, self.plane_position,
                                self.window_width, self.window_level,
                                self.interp_order, row_range=row_range)
        return render_slab(self.model, self.plane, self.plane_position,
                           self.thickness, self.window_width,
                           self.window_level, self.interp_order,
                           row_range=row_range)

    # -- annotations -----------------------------------------------------------
    def add_annotation(self, p0: Sequence[float], p1: Sequence[float],
                       true_length: Optional[float] = None) -> Annotation:
        annotation = make_annotation(
            self.model, self._next_annotation_id, self.plane,
            self.plane_position, p0, p1, self.interp_order,
            true_length=true_length)
        self.annotations.append(annotation)
        self._next_annotation_id += 1
        return annotation

    def reading(self, annotation_id: int) -> dict:
        for annotation in self.annotations:
            if annotation.id == annotation_id:
                return annotation.reading()
        raise VolViewError(ErrorCode.E_RANGE, f"annotation[{annotation_id}]",
                           "no such annotation", annotation_id=annotation_id)

    def annotation_on_plane(self, annotation_id: int) -> dict:
        """Re-intersect a stored annotation with the *current* plane."""
        for annotation in self.annotations:
            if annotation.id == annotation_id:
                return project_to_plane(self.model, annotation, self.plane,
                                        self.plane_position)
        raise VolViewError(ErrorCode.E_RANGE, f"annotation[{annotation_id}]",
                           "no such annotation", annotation_id=annotation_id)

    def check_accuracy(self, annotation_id: int, true_length: float) -> dict:
        """Deviation of an annotation from analytic truth plus the bound."""
        for annotation in self.annotations:
            if annotation.id == annotation_id:
                deviation = abs(annotation.world_length - float(true_length))
                bound = accuracy_bound(self.model, self.interp_order)
                if deviation > bound:
                    raise VolViewError(
                        ErrorCode.E_ACC, f"annotation[{annotation_id}]",
                        f"measured {annotation.world_length:.6f} mm deviates "
                        f"{deviation:.6f} mm from truth, bound {bound:.6f} mm",
                        annotation_id=annotation_id,
                        measured=annotation.world_length,
                        deviation=deviation, bound=bound)
                return {"annotation_id": annotation_id,
                        "measured": annotation.world_length,
                        "true_length": float(true_length),
                        "deviation": deviation, "bound": bound}
        raise VolViewError(ErrorCode.E_RANGE, f"annotation[{annotation_id}]",
                           "no such annotation", annotation_id=annotation_id)

    # -- persistence -------------------------------------------------------------
    def snapshot_dict(self) -> dict:
        return {
            "schema_version": SNAPSHOT_SCHEMA_VERSION,
            "session_id": self.session_id,
            "volume_id": self.model.manifest.volume_id,
            "content_hash": self.model.content_hash,
            "params": {
                "window_width": self.window_width,
                "window_level": self.window_level,
                "plane": self.plane,
                "plane_position": self.plane_position,
                "thickness": self.thickness,
                "interp_order": self.interp_order,
            },
            "annotations": [
                {**a.reading(), "plane_position": a.plane_position,
                 "true_length": a.true_length}
                for a in self.annotations
            ],
        }

    def commit(self, sequence: Optional[int] = None,
               max_task_pixels: Optional[int] = None) -> int:
        """Render the current plane and atomically persist the snapshot."""
        if sequence is None:
            sequence = self.store.allocate_sequence()
        image = self.render(max_task_pixels=max_task_pixels)
        self.store.save_ordered(sequence, self.session_id,
                                self.snapshot_dict(), image)
        return sequence
