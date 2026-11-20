"""Headless frame loop used for validation and as the interactive model.

The same loop structure drives a real window frontend: each frame reads the
engine's current committed snapshot once, renders it, samples latency and
polls job progress. Edit work happens on the worker thread, so rotation and
zoom inputs are processed every frame regardless of edit duration; jobs over
the declared threshold surface progress instead of freezing.
"""

from __future__ import annotations

from ..engine import EditEngine
from ..telemetry import LatencyMonitor
from .camera import OrbitCamera


def run_headless_frames(engine: EditEngine, renderer, frames: int,
                        monitor: LatencyMonitor | None = None,
                        inputs: list | None = None,
                        progress_threshold_s: float = 0.5,
                        frame_budget_s: float = 1 / 30.0) -> dict:
    """Render ``frames`` frames while an optional edit runs in the background.

    ``inputs`` is an optional list of ``(frame_index, ("rotate", dy, dp))`` or
    ``(frame_index, ("zoom", factor))`` events proving the render thread stays
    responsive while the worker thread computes.
    """
    monitor = monitor or LatencyMonitor()
    camera = OrbitCamera()
    inputs = inputs or []
    input_by_frame: dict[int, list] = {}
    for item in inputs:
        input_by_frame.setdefault(item[0], []).append(item[1])

    progress_seen = []
    for frame_index in range(frames):
        for kind, *args in input_by_frame.get(frame_index, []):
            monitor.record_input()
            if kind == "rotate":
                camera.rotate(args[0], args[1])
            elif kind == "zoom":
                camera.zoom(args[0])

        cloud = engine.current()  # one complete committed snapshot per frame
        camera.fit(cloud.bbox) if frame_index == 0 else None
        renderer.render(cloud, camera)
        monitor.record_frame(frame_budget_s * 1000.0)

        state = engine.progress()
        if state.get("running"):
            progress_seen.append(dict(state))

    return {
        "frames": frames,
        "progress_samples": progress_seen,
        "stats": monitor.stats(),
    }
