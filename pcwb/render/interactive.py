"""Optional interactive OpenGL window (GLFW).

This module is imported lazily and only used when a display plus the ``glfw``
wheel are available. In environments without windowing native libraries (the
validation container here has EGL but no freeglut/GLFW), the headless EGL /
CPU renderers in :mod:`pcwb.render` are used instead, which implement the same
snapshot-per-frame rendering contract.
"""

from __future__ import annotations


class InteractiveWindow:
    """Orbit/zoom GLFW window rendering engine snapshots each frame."""

    def __init__(self, width: int = 960, height: int = 720,
                 title: str = "pcwb point-cloud workbench"):
        try:
            import glfw  # noqa: F401
        except Exception as exc:  # pragma: no cover - environment dependent
            raise RuntimeError(
                "interactive window requires the glfw wheel and a display; "
                "use the headless EGL/CPU renderers otherwise") from exc
        self._glfw = glfw
        self.width, self.height = width, height
        self.title = title
        self._window = None

    def run(self, engine, camera, on_frame=None) -> None:  # pragma: no cover
        glfw = self._glfw
        if not glfw.init():
            raise RuntimeError("glfw.init failed")
        try:
            self._window = glfw.create_window(
                self.width, self.height, self.title, None, None)
            if not self._window:
                raise RuntimeError("glfw.create_window failed")
            glfw.make_context_current(self._window)
            from .gl_renderer import EGLRenderer  # reuse GL program setup
            renderer = EGLRenderer(self.width, self.height)

            def on_cursor(_w, x, y):
                camera.rotate(x * 0.005, y * 0.005)

            def on_scroll(_w, _x, dy):
                camera.zoom(1.0 + 0.1 * dy)

            glfw.set_cursor_pos_callback(self._window, on_cursor)
            glfw.set_scroll_callback(self._window, on_scroll)
            while not glfw.window_should_close(self._window):
                cloud = engine.current()
                renderer.render(cloud, camera)
                if on_frame is not None:
                    on_frame(engine.progress())
                glfw.swap_buffers(self._window)
                glfw.poll_events()
            renderer.close()
        finally:
            glfw.terminate()
