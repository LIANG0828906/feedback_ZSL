"""PyQt5 preview window with fixed refresh cycle and GL/offscreen fallback."""

import sys

from PyQt5 import QtCore, QtGui, QtWidgets

from .preview import PreviewService


class PreviewWidget(QtWidgets.QLabel):
    refresh_tick = QtCore.pyqtSignal()

    def __init__(self, preview_service, refresh_ms=500, parent=None):
        super().__init__(parent)
        self.preview_service = preview_service
        self._shown_version = None
        self.setMinimumSize(640, 480)
        self.setAlignment(QtCore.Qt.AlignCenter)
        self.setText("no committed version yet")
        self.timer = QtCore.QTimer(self)
        self.timer.setInterval(refresh_ms)
        self.timer.timeout.connect(self._refresh)
        self.timer.start()

    def _refresh(self):
        snapshot = self.preview_service.current()
        if snapshot is None or snapshot.version == self._shown_version:
            return
        image = QtGui.QImage(
            snapshot.canvas.tobytes(),
            snapshot.canvas.shape[1],
            snapshot.canvas.shape[0],
            snapshot.canvas.strides[0],
            QtGui.QImage.Format_RGBA8888,
        ).copy()
        self.setPixmap(QtGui.QPixmap.fromImage(image))
        self._shown_version = snapshot.version
        self.setWindowTitle(f"NPR preview v{snapshot.version}")


def has_gl_context():
    context = QtGui.QOpenGLContext()
    return context.create()


def run_viewer(workdir, refresh_ms=500, force_offscreen=False):
    """Blocking GUI entry; falls back to offscreen rasterization without GL."""
    import os
    import subprocess
    import sys

    from .pipeline import Pipeline

    def platform_works(platform):
        probe = subprocess.run(
            [
                sys.executable,
                "-c",
                "from PyQt5 import QtWidgets; QtWidgets.QApplication([])",
            ],
            env={**os.environ, "QT_QPA_PLATFORM": platform},
            capture_output=True,
        )
        return probe.returncode == 0

    if force_offscreen or not has_gl_context() or not platform_works(
        os.environ.get("QT_QPA_PLATFORM", "wayland")
    ):
        os.environ["QT_QPA_PLATFORM"] = "offscreen"
        QtCore.QCoreApplication.setAttribute(QtCore.Qt.AA_UseSoftwareOpenGL)
        QtWidgets.QApplication.setAttribute(QtCore.Qt.AA_UseSoftwareOpenGL)
    app = QtWidgets.QApplication(sys.argv)
    pipeline = Pipeline(workdir)
    manifest, _ = pipeline.recover()
    service = PreviewService(pipeline.store)
    if manifest is not None:
        service.publish(manifest)
    widget = PreviewWidget(service, refresh_ms=refresh_ms)
    widget.show()
    return app.exec_()


if __name__ == "__main__":
    raise SystemExit(run_viewer(sys.argv[1]))
