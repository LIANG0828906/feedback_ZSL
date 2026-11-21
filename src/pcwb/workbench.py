"""Interactive OpenGL workbench with an offscreen/software fallback."""

from __future__ import annotations

import math
import os
import time
from dataclasses import dataclass
from typing import Any

import numpy as np

from .async_edit import AsyncEditController
from .cache import OperationCache
from .diff import VersionDiff, diff_clouds
from .errors import BusinessError, EditBusy
from .math3d import OrbitCamera
from .metrics import InteractionMetrics
from .model import PointCloudSnapshot
from .repository import PointCloudRepository
from .selection import (
    Selection,
    select_screen_rectangle,
    select_sphere,
)
from .trace import RuntimeMetricsLog


@dataclass(slots=True)
class WorkbenchOptions:
    headless: bool = False
    frames: int = 0
    width: int = 1100
    height: int = 760
    frame_budget_ms: float = 16.667
    progress_threshold_ms: float = 250.0
    metrics_path: str | None = None
    auto_smooth: bool = False
    scripted_input: bool = False


class Workbench:
    def __init__(
        self,
        repository: PointCloudRepository,
        options: WorkbenchOptions,
    ) -> None:
        self.repository = repository
        self.options = options
        self.controller = AsyncEditController(
            repository,
            cache=OperationCache(max_entries=16),
            progress_threshold_ms=options.progress_threshold_ms,
        )
        self.metrics = InteractionMetrics(frame_budget_ms=options.frame_budget_ms)
        self.runtime_log = (
            RuntimeMetricsLog(options.metrics_path) if options.metrics_path else None
        )
        self.camera = OrbitCamera()
        self.snapshot: PointCloudSnapshot | None = None
        self.selection: Selection | None = None
        self.selection_mode: str | None = None
        self.dragging_selection = False
        self.drag_start = (0.0, 0.0)
        self.drag_end = (0.0, 0.0)
        self.orbiting = False
        self.last_cursor = (0.0, 0.0)
        self.compare_version: int | None = None
        self.diff: VersionDiff | None = None
        self.auto_edit_started = False
        self.rendered_frames = 0
        self._last_title = ""
        self._last_title_time = 0.0
        self._recorded_job_key: tuple[Any, ...] | None = None
        self._close_requested = False
        self._offscreen_non_background = 0

    def _versions_sorted(self) -> list[int]:
        return sorted(entry["version"] for entry in self.repository.versions())

    def _choose_compare_version(self) -> int | None:
        versions = self._versions_sorted()
        if not self.snapshot or len(versions) < 2:
            return None
        current = self.snapshot.version
        if current in versions:
            index = versions.index(current)
            if index > 0:
                return versions[index - 1]
        return versions[-1] if versions[-1] != current else None

    def _status_text(self) -> str:
        assert self.snapshot is not None
        status = self.controller.status()
        selection_text = "none"
        if (
            self.selection is not None
            and self.selection.version_hash == self.snapshot.content_hash
        ):
            selection_text = f"{self.selection.count}"
        diff_text = ""
        if self.diff is not None:
            diff_text = (
                f" diff(+{self.diff.added}/-{self.diff.removed}/"
                f"color {self.diff.color_changed})"
            )
        mode_text = self.selection_mode or "orbit"
        progress_text = ""
        if (
            status.state in {"running", "committing", "cancelling"}
            and status.show_progress
        ):
            progress_text = (
                f" | {status.operator} {status.phase} "
                f"{int(status.fraction * 100):3d}% {status.elapsed_ms:.0f}ms"
            )
        elif status.state == "failed" and status.error:
            progress_text = f" | {status.error.get('code', 'ERROR')}"
        return (
            f"PCWB v{self.snapshot.version} {self.snapshot.short_hash} "
            f"points={self.snapshot.count} sel={selection_text} mode={mode_text}"
            f"{diff_text}{progress_text}  [R]ect [S]phere [C]rop [X]remove "
            f"[G]mooth [V]oxel [O]color [B]ack [J]diff [Esc]cancel [Q]uit"
        )

    def _set_title(self, glfw: Any, window: Any) -> None:
        now = time.monotonic()
        if now - self._last_title_time < 0.1:
            return
        title = self._status_text()
        if title != self._last_title:
            glfw.set_window_title(window, title)
            self._last_title = title
            self._last_title_time = now
            print(f"[status] {title}", flush=True)

    def _sync_frame_snapshot(self, renderer: Any) -> None:
        snapshot = self.controller.display_snapshot()
        if self.snapshot is None:
            self.snapshot = snapshot
            self.camera.set_scene(
                snapshot.cloud.bounds.center, snapshot.cloud.bounds.diagonal
            )
            self.compare_version = self._choose_compare_version()
            renderer.set_current(snapshot.cloud)
            print(
                f"[load] version={snapshot.version} hash={snapshot.short_hash} "
                f"points={snapshot.count} bounds={snapshot.cloud.bounds.to_dict()}",
                flush=True,
            )
            return
        if (
            snapshot.version != self.snapshot.version
            or snapshot.content_hash != self.snapshot.content_hash
        ):
            self.snapshot = snapshot
            renderer.set_current(snapshot.cloud)
            renderer.set_ghost(None)
            self.selection = None
            self.diff = None
            self.compare_version = self._choose_compare_version()
            print(
                f"[publish] version={snapshot.version} hash={snapshot.short_hash} "
                f"points={snapshot.count}",
                flush=True,
            )

    def _rectangle_overlay(self, glfw: Any, window: Any) -> list[tuple[float, float]]:
        width, height = glfw.get_window_size(window)
        if width <= 0 or height <= 0:
            return []
        x0, y0 = self.drag_start
        x1, y1 = self.drag_end
        points = [(x0, y0), (x1, y0), (x1, y1), (x0, y1)]
        return [((2.0 * x / width) - 1.0, 1.0 - (2.0 * y / height)) for x, y in points]

    def _sphere_overlay(self, glfw: Any, window: Any) -> list[tuple[float, float]]:
        width, height = glfw.get_window_size(window)
        if width <= 0 or height <= 0:
            return []
        x0, y0 = self.drag_start
        x1, y1 = self.drag_end
        radius = math.hypot(x1 - x0, y1 - y0)
        result = []
        for index in range(48):
            angle = 2.0 * math.pi * index / 48
            x = x0 + radius * math.cos(angle)
            y = y0 + radius * math.sin(angle)
            result.append(((2.0 * x / width) - 1.0, 1.0 - (2.0 * y / height)))
        return result

    def _apply_selection(self, glfw: Any, window: Any) -> None:
        assert self.snapshot is not None
        width, height = glfw.get_window_size(window)
        if self.selection_mode == "rectangle":
            self.selection = select_screen_rectangle(
                self.snapshot,
                self.camera.view_projection(width, height),
                self.drag_start[0],
                self.drag_start[1],
                self.drag_end[0],
                self.drag_end[1],
                width,
                height,
            )
        elif self.selection_mode == "sphere":
            center = self.camera.screen_world(
                self.drag_start[0], self.drag_start[1], width, height
            )
            edge = self.camera.screen_world(
                self.drag_end[0], self.drag_end[1], width, height
            )
            radius = float(np.linalg.norm(np.asarray(edge) - np.asarray(center)))
            if radius <= 1e-9:
                return
            self.selection = select_sphere(self.snapshot, center, radius)
        if self.selection is not None:
            print(
                f"[selection] type={self.selection_mode} "
                f"count={self.selection.count} "
                f"bounds={None if self.selection.bounds is None else self.selection.bounds.to_dict()}",
                flush=True,
            )

    def _record_input(self, started_ns: int) -> None:
        self.metrics.record_input_latency(
            (time.perf_counter_ns() - started_ns) / 1_000_000.0
        )

    def _submit(self, operator: str, params: dict[str, Any]) -> None:
        if self.snapshot is None or self.selection is None:
            print("[edit] no selection", flush=True)
            return
        if self.selection.version_hash != self.snapshot.content_hash:
            print("[edit] selection is stale", flush=True)
            return
        try:
            self.controller.submit(operator, params, self.selection)
            print(f"[edit] submitted {operator} {params}", flush=True)
        except EditBusy as exc:
            print(f"[edit] busy: {exc}", flush=True)
        except BusinessError as exc:
            print(f"[edit] rejected {exc.code}: {exc.message}", flush=True)

    def _apply_key(self, glfw: Any, window: Any, key: int) -> None:
        if key == glfw.KEY_Q:
            self._close_requested = True
            return
        if key == glfw.KEY_ESCAPE:
            if self.controller.cancel():
                print("[edit] cancellation requested", flush=True)
            else:
                self.selection = None
                self.selection_mode = None
            return
        if key == glfw.KEY_R:
            self.selection_mode = "rectangle"
            return
        if key == glfw.KEY_S:
            self.selection_mode = "sphere"
            return
        if self.snapshot is None:
            return
        diagonal = max(self.snapshot.cloud.bounds.diagonal, 1e-6)
        if key == glfw.KEY_C:
            self._submit("crop", {"mode": "keep"})
        elif key == glfw.KEY_X:
            self._submit("crop", {"mode": "remove"})
        elif key == glfw.KEY_G:
            self._submit(
                "smooth",
                {"radius": round(diagonal * 0.012, 6), "iterations": 2},
            )
        elif key == glfw.KEY_V:
            self._submit(
                "downsample",
                {"mode": "voxel", "voxel_size": round(diagonal * 0.015, 6)},
            )
        elif key == glfw.KEY_O:
            self._submit("color_overwrite", {"rgb": [240, 140, 40]})
        elif key == glfw.KEY_B and self.snapshot.parent_version > 0:
            try:
                before = self.snapshot.version
                self.repository.checkout(self.snapshot.parent_version)
                self.controller.sync_current()
                self.diff = None
                print(
                    f"[rollback] {before} -> {self.snapshot.parent_version}",
                    flush=True,
                )
            except BusinessError as exc:
                print(f"[rollback] {exc.code}: {exc.message}", flush=True)
        elif key == glfw.KEY_LEFT_BRACKET:
            versions = self._versions_sorted()
            if versions:
                current = self.compare_version or versions[-1]
                index = (
                    versions.index(current)
                    if current in versions
                    else len(versions) - 1
                )
                self.compare_version = versions[max(0, index - 1)]
                print(f"[diff] compare version {self.compare_version}", flush=True)
        elif key == glfw.KEY_RIGHT_BRACKET:
            versions = self._versions_sorted()
            if versions:
                current = self.compare_version or versions[0]
                index = versions.index(current) if current in versions else 0
                self.compare_version = versions[min(len(versions) - 1, index + 1)]
                print(f"[diff] compare version {self.compare_version}", flush=True)
        elif (
            key == glfw.KEY_J
            and self.compare_version is not None
            and self.snapshot is not None
        ):
            try:
                old = self.repository.load_version(self.compare_version)
                self.diff = diff_clouds(old.cloud, self.snapshot.cloud)
                print(f"[diff] {self.diff.to_dict()}", flush=True)
            except BusinessError as exc:
                print(f"[diff] {exc.code}: {exc.message}", flush=True)

    def _bind_callbacks(self, glfw: Any, window: Any, renderer: Any) -> None:
        def mouse_button(_window: Any, button: int, action: int, _mods: int) -> None:
            started = time.perf_counter_ns()
            try:
                if button != glfw.MOUSE_BUTTON_LEFT:
                    return
                if action == glfw.PRESS:
                    self.last_cursor = glfw.get_cursor_pos(window)
                    if self.selection_mode is not None:
                        self.dragging_selection = True
                        self.drag_start = self.last_cursor
                        self.drag_end = self.last_cursor
                    else:
                        self.orbiting = True
                elif action == glfw.RELEASE:
                    if self.dragging_selection:
                        self.drag_end = glfw.get_cursor_pos(window)
                        self.dragging_selection = False
                        self._apply_selection(glfw, window)
                    self.orbiting = False
            finally:
                self._record_input(started)

        def cursor(_window: Any, x: float, y: float) -> None:
            started = time.perf_counter_ns()
            try:
                if self.dragging_selection:
                    self.drag_end = (x, y)
                elif self.orbiting:
                    delta_x = x - self.last_cursor[0]
                    delta_y = y - self.last_cursor[1]
                    self.camera.orbit(delta_x, delta_y)
                    self.last_cursor = (x, y)
            finally:
                self._record_input(started)

        def scroll(_window: Any, _xoff: float, yoff: float) -> None:
            started = time.perf_counter_ns()
            try:
                self.camera.zoom(yoff)
            finally:
                self._record_input(started)

        def key(
            _window: Any, key_value: int, _scancode: int, action: int, _mods: int
        ) -> None:
            if action != glfw.PRESS:
                return
            started = time.perf_counter_ns()
            try:
                self._apply_key(glfw, window, key_value)
            finally:
                self._record_input(started)

        glfw.set_mouse_button_callback(window, mouse_button)
        glfw.set_cursor_pos_callback(window, cursor)
        glfw.set_scroll_callback(window, scroll)
        glfw.set_key_callback(window, key)

    def _maybe_auto_edit(self) -> None:
        if (
            not self.options.auto_smooth
            or self.auto_edit_started
            or self.snapshot is None
        ):
            return
        if self.controller.status().state not in {
            "idle",
            "succeeded",
            "failed",
            "cancelled",
        }:
            return
        selection = select_sphere(
            self.snapshot,
            self.snapshot.cloud.bounds.center,
            self.snapshot.cloud.bounds.diagonal * 0.25,
        )
        if selection.is_empty:
            return
        self.selection = selection
        self.auto_edit_started = True
        self._submit(
            "smooth",
            {
                "radius": round(self.snapshot.cloud.bounds.diagonal * 0.012, 6),
                "iterations": 3,
            },
        )

    def run(self) -> dict[str, Any]:
        try:
            return self._run_gl()
        except Exception as exc:  # noqa: BLE001 - fallback for any context failure
            if not self.options.headless:
                print(f"[render] OpenGL window failed: {exc}", flush=True)
                print("[render] falling back to software snapshot loop", flush=True)
                return self._run_software_fallback(exc)
            print(f"[render] offscreen OpenGL unavailable: {exc}", flush=True)
            print("[render] using software snapshot fallback", flush=True)
            return self._run_software_fallback(exc)

    def _run_gl(self) -> dict[str, Any]:
        # PyOpenGL's automatic platform detection can select EGL on a mixed
        # X11/Wayland host while GLFW creates a GLX context.  Pin GLX when an
        # X display is present; otherwise keep the EGL/Wayland auto-detection.
        if os.environ.get("DISPLAY") and not os.environ.get("PYOPENGL_PLATFORM"):
            os.environ["PYOPENGL_PLATFORM"] = "glx"
        import glfw
        from OpenGL import GL

        from .gl_renderer import PointCloudGLRenderer

        if not glfw.init():
            raise RuntimeError("glfw.init failed")
        window = None
        renderer = None
        try:
            glfw.window_hint(glfw.CONTEXT_VERSION_MAJOR, 3)
            glfw.window_hint(glfw.CONTEXT_VERSION_MINOR, 3)
            glfw.window_hint(glfw.OPENGL_PROFILE, glfw.OPENGL_CORE_PROFILE)
            glfw.window_hint(glfw.DOUBLEBUFFER, glfw.TRUE)
            glfw.window_hint(glfw.RESIZABLE, glfw.TRUE)
            glfw.window_hint(
                glfw.VISIBLE, glfw.FALSE if self.options.headless else glfw.TRUE
            )
            window = glfw.create_window(
                self.options.width,
                self.options.height,
                "PCWB point-cloud workbench",
                None,
                None,
            )
            if window is None:
                error = glfw.get_error()
                raise RuntimeError(f"cannot create OpenGL context: {error}")
            glfw.make_context_current(window)
            glfw.swap_interval(0 if self.options.headless else 1)
            version = GL.glGetString(GL.GL_VERSION)
            renderer_name = GL.glGetString(GL.GL_RENDERER)
            if isinstance(version, bytes):
                version = version.decode("utf-8", "replace")
            if isinstance(renderer_name, bytes):
                renderer_name = renderer_name.decode("utf-8", "replace")
            print(
                f"[render] OpenGL={version} renderer={renderer_name} "
                f"mode={'offscreen' if self.options.headless else 'window'}",
                flush=True,
            )
            renderer = PointCloudGLRenderer()
            self._bind_callbacks(glfw, window, renderer)
            # Warm up shader compilation, buffer upload, and the first driver
            # frame outside the measured interaction loop.
            glfw.poll_events()
            self._sync_frame_snapshot(renderer)
            warm_width, warm_height = glfw.get_framebuffer_size(window)
            renderer.begin_frame(warm_width, warm_height)
            renderer.draw(self.camera.view_projection(warm_width, warm_height))
            glfw.swap_buffers(window)
            self._set_title(glfw, window)
            frame_limit = self.options.frames if self.options.headless else 0
            last_progress_print = 0.0
            while not glfw.window_should_close(window) and not self._close_requested:
                frame_started = time.perf_counter_ns()
                glfw.poll_events()
                if self.options.scripted_input:
                    input_started = time.perf_counter_ns()
                    self.camera.orbit(0.35, 0.0)
                    self.metrics.record_input_latency(
                        (time.perf_counter_ns() - input_started) / 1_000_000.0
                    )
                self._sync_frame_snapshot(renderer)
                self._maybe_auto_edit()
                assert self.snapshot is not None
                width, height = glfw.get_framebuffer_size(window)
                view_projection = self.camera.view_projection(width, height)
                renderer.begin_frame(width, height)
                selected = ()
                if (
                    self.selection is not None
                    and self.selection.version_hash == self.snapshot.content_hash
                ):
                    selected = self.selection.indices
                added = () if self.diff is None else self.diff.added_indices
                changed = () if self.diff is None else self.diff.color_changed_indices
                renderer.draw(
                    view_projection,
                    selected_indices=selected,
                    added_indices=added,
                    changed_indices=changed,
                )
                if self.dragging_selection:
                    points = (
                        self._rectangle_overlay(glfw, window)
                        if self.selection_mode == "rectangle"
                        else self._sphere_overlay(glfw, window)
                    )
                    renderer.overlay.draw(
                        points,
                        color=(1.0, 0.78, 0.12, 1.0),
                        mode=None
                        if self.selection_mode == "rectangle"
                        else GL.GL_LINE_LOOP,
                    )
                if (
                    self.options.headless
                    and frame_limit > 0
                    and (self.rendered_frames == max(0, frame_limit - 1))
                ):
                    sample_width = min(128, max(1, width))
                    sample_height = min(128, max(1, height))
                    pixels = np.empty((sample_height, sample_width, 4), dtype=np.uint8)
                    GL.glReadPixels(
                        max(0, (width - sample_width) // 2),
                        max(0, (height - sample_height) // 2),
                        sample_width,
                        sample_height,
                        GL.GL_RGBA,
                        GL.GL_UNSIGNED_BYTE,
                        pixels,
                    )
                    self._offscreen_non_background = int(
                        np.count_nonzero(np.any(pixels[:, :, :3] > 25, axis=2))
                    )
                glfw.swap_buffers(window)
                frame_ms = (time.perf_counter_ns() - frame_started) / 1_000_000.0
                self.metrics.record_frame(frame_ms)
                self._set_title(glfw, window)
                self.rendered_frames += 1
                status = self.controller.status()
                now = time.monotonic()
                job_key = (
                    status.base_hash,
                    status.operator,
                    status.state,
                    status.result_version,
                )
                if (
                    status.state in {"succeeded", "failed", "cancelled"}
                    and job_key != self._recorded_job_key
                ):
                    self.metrics.record_edit_job(status.elapsed_ms)
                    self._recorded_job_key = job_key
                if status.show_progress and now - last_progress_print >= 0.2:
                    print(f"[progress] {status.to_dict()}", flush=True)
                    last_progress_print = now
                if frame_limit and self.rendered_frames >= frame_limit:
                    status = self.controller.status()
                    edit_active = status.state in {
                        "running",
                        "committing",
                        "cancelling",
                    }
                    if not (self.options.auto_smooth and edit_active):
                        break
            status = self.controller.status()
            metrics_summary = {
                "renderer": "opengl",
                "headless": self.options.headless,
                "frames": self.rendered_frames,
                "final_version": self.controller.display_snapshot().version,
                "final_hash": self.controller.display_snapshot().content_hash,
                "offscreen_non_background_pixels": self._offscreen_non_background,
                "metrics": self.metrics.summary(),
                "last_edit": status.to_dict(),
            }
            if self.runtime_log is not None:
                self.runtime_log.append({"type": "session", **metrics_summary})
            return metrics_summary
        finally:
            if renderer is not None:
                renderer.destroy()
            if window is not None:
                glfw.destroy_window(window)
            glfw.terminate()
            self.controller.close()

    def _run_software_fallback(self, reason: Exception) -> dict[str, Any]:
        frames = max(1, self.options.frames or 30)
        for _ in range(frames):
            start = time.perf_counter_ns()
            snapshot = self.controller.display_snapshot()
            self.snapshot = snapshot
            self.metrics.record_frame((time.perf_counter_ns() - start) / 1_000_000.0)
            time.sleep(0.001)
        summary = {
            "renderer": "software_snapshot_fallback",
            "reason": str(reason),
            "frames": frames,
            "final_version": self.snapshot.version if self.snapshot else 0,
            "final_hash": self.snapshot.content_hash if self.snapshot else None,
            "metrics": self.metrics.summary(),
        }
        if self.runtime_log is not None:
            self.runtime_log.append({"type": "session", **summary})
        self.controller.close()
        return summary


def run_workbench(
    repository: PointCloudRepository | str | os.PathLike[str],
    options: WorkbenchOptions | None = None,
) -> dict[str, Any]:
    repo = (
        repository
        if isinstance(repository, PointCloudRepository)
        else PointCloudRepository(repository)
    )
    return Workbench(repo, options or WorkbenchOptions()).run()
