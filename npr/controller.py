"""Background recompute controller with single-task and cancel boundaries."""

import threading

from .errors import PipelineError
from .preview import PreviewService
from .pipeline import Pipeline


class RecomputeController:
    def __init__(self, workdir):
        self.pipeline = Pipeline(workdir)
        self.preview = PreviewService(self.pipeline.store)
        self._thread = None
        self._cancel = None
        self._guard = threading.Lock()
        self._last_error = None
        self._last_manifest = None

    def submit(self, scene, outline_p, shadow_p, smear_p, on_done=None):
        """Start one recompute; E_BUSY while a task is already running."""
        with self._guard:
            if self._thread is not None and self._thread.is_alive():
                raise PipelineError.busy()
            self._cancel = threading.Event()
            self._last_error = None
            thread = threading.Thread(
                target=self._run,
                args=(scene, outline_p, shadow_p, smear_p, on_done),
                name="npr-recompute",
                daemon=True,
            )
            self._thread = thread
        thread.start()
        return thread

    def _run(self, scene, outline_p, shadow_p, smear_p, on_done):
        try:
            manifest, _stats = self.pipeline.recompute(
                scene, outline_p, shadow_p, smear_p, cancel_event=self._cancel
            )
            self._last_manifest = manifest
            self.preview.publish(manifest)
            if on_done is not None:
                on_done(manifest, None)
        except PipelineError as exc:
            self._last_error = exc
            if on_done is not None:
                on_done(None, exc)

    def cancel(self, timeout=5.0):
        with self._guard:
            event = self._cancel
            thread = self._thread
        if event is not None:
            event.set()
        if thread is not None:
            thread.join(timeout=timeout)
        return thread is None or not thread.is_alive()

    def wait_idle(self, timeout=30.0):
        thread = self._thread
        if thread is not None:
            thread.join(timeout=timeout)
        return self._last_manifest, self._last_error

    @property
    def running(self):
        return self._thread is not None and self._thread.is_alive()
