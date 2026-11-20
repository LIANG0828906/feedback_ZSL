"""High-level workbench facade combining store, engine, render and telemetry."""

from __future__ import annotations

import os

import numpy as np

from .cloud import PointCloud
from .diff import CloudDiff, diff_clouds
from .engine import EditEngine
from .format import read_cloud, write_cloud
from .selection import Selection, box_select, sphere_select
from .telemetry import LatencyMonitor, TrajectoryLog
from .versions import VersionStore


class Workbench:
    """Facade used by both the scripted demo and an interactive frontend."""

    def __init__(self, store_dir: str, trajectory_path: str | None = None,
                 monitor: LatencyMonitor | None = None):
        self.store = VersionStore(store_dir)
        self.engine = EditEngine(self.store)
        self.monitor = monitor or LatencyMonitor()
        self.trajectory = (TrajectoryLog(trajectory_path)
                           if trajectory_path else None)
        self._selection: Selection | None = None

    # ------------------------------------------------------------------ io
    def import_cloud(self, path: str) -> PointCloud:
        xyz, rgb, bbox, sha = read_cloud(path)
        cloud = self.store.commit(
            xyz, rgb,
            record={"operator": "import", "params": {"path": os.path.basename(path)},
                    "input_version": None, "input_hash": None,
                    "input_count": 0, "selected_count": 0,
                    "output_count": int(xyz.shape[0])})
        self.engine.publish(cloud)
        self._log({"event": "commit", **self._record_view(cloud)})
        return cloud

    def export_cloud(self, cloud: PointCloud, path: str) -> str:
        return write_cloud(path, cloud.xyz, cloud.rgb, cloud.bbox)

    # ------------------------------------------------------------------ selection
    def select_box(self, lower, upper) -> Selection:
        cloud = self.engine.current()
        self._selection = box_select(cloud, np.asarray(lower, dtype=np.float64),
                                     np.asarray(upper, dtype=np.float64))
        self._log_selection("box")
        return self._selection

    def select_sphere(self, center, radius: float) -> Selection:
        cloud = self.engine.current()
        self._selection = sphere_select(
            cloud, np.asarray(center, dtype=np.float64), float(radius))
        self._log_selection("sphere")
        return self._selection

    @property
    def selection(self) -> Selection | None:
        return self._selection

    def _log_selection(self, kind: str) -> None:
        sel = self._selection
        cloud = self.engine.current()
        self._log({
            "event": "selection",
            "kind": kind,
            "version": cloud.version,
            "cloud_hash": cloud.sha256,
            "selected_count": sel.count,
            "selection_bbox": np.asarray(sel.bbox(cloud)).round(6).tolist(),
        })

    # ------------------------------------------------------------------ edits
    def submit_edit(self, operator: str, params: dict | None = None):
        if self._selection is None:
            raise RuntimeError("no active selection")
        job = self.engine.submit(operator, self._selection, params)
        return job

    def wait_edit(self, timeout: float | None = None) -> bool:
        ok = self.engine.wait(timeout)
        outcome = self.engine.last_outcome
        if outcome is not None and outcome.record is not None:
            # Emit submit + completion together from this single thread so
            # trajectory line ordering is deterministic despite the worker.
            self._log({
                "event": "edit_submit",
                "job_id": outcome.job_id,
                "operator": outcome.record.get("operator"),
                "params": outcome.record.get("params"),
                "input_version": outcome.record.get("input_version"),
                "input_hash": outcome.record.get("input_hash"),
            })
            if outcome.ok:
                self._log({"event": "commit",
                           **self._record_view(outcome.cloud)})
            else:
                self._log({"event": "edit_failed", **outcome.record})
        return ok

    def cancel_edit(self) -> None:
        self.engine.cancel()
        self.engine.wait(timeout=5.0)
        self._log({"event": "edit_cancelled",
                   "job_id": self.engine.last_outcome.job_id
                   if self.engine.last_outcome else None})

    # ------------------------------------------------------------------ versions
    def checkout(self, version: int) -> PointCloud:
        cloud = self.store.checkout(version)
        self.engine.publish(cloud)
        self._selection = None
        self._log({"event": "checkout", "version": cloud.version,
                   "cloud_hash": cloud.sha256})
        return cloud

    def diff_versions(self, old_version: int, new_version: int,
                      grid: float = 1e-6) -> CloudDiff:
        old = self.store.load(old_version)
        new = self.store.load(new_version)
        result = diff_clouds(old, new, grid=grid)
        self._log({"event": "diff", "old_version": old_version,
                   "new_version": new_version, **result.counts})
        return result

    # ------------------------------------------------------------------ helpers
    def _record_view(self, cloud: PointCloud) -> dict:
        entry = self.store.entry(cloud.version)
        return {
            "version": cloud.version,
            "operator": entry.record.get("operator"),
            "params": entry.record.get("params"),
            "input_version": entry.parent_version,
            "input_hash": entry.record.get("input_hash"),
            "output_hash": cloud.sha256,
            "output_count": cloud.count,
        }

    def _log(self, event: dict) -> None:
        if self.trajectory is not None:
            self.trajectory.emit(event)

    def shutdown(self) -> None:
        self.engine.shutdown()
