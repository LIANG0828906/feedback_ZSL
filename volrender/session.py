"""Single-flight rendering session used by the desktop application."""

from __future__ import annotations

import threading
from pathlib import Path
from typing import Any, Callable, TypeVar

from .errors import E_BUSY, VolRenderError
from .render import (
    LightDriftReport,
    ModeSwitchReport,
    PrecisionComparison,
    RenderResult,
    VolumeRenderer,
)
from .volume import MappingChange


T = TypeVar("T")


class RenderSession:
    """Allow exactly one in-flight render and preserve the last good state."""

    def __init__(self, renderer: VolumeRenderer) -> None:
        self.renderer = renderer
        self._lock = threading.Lock()
        self._busy = False
        self.last_good_result: RenderResult | None = renderer.last_good_result

    @property
    def busy(self) -> bool:
        return self._busy

    def _submit(self, action: Callable[[], T]) -> T:
        if not self._lock.acquire(blocking=False):
            raise VolRenderError(
                E_BUSY,
                "a render task is already in progress",
            )
        self._busy = True
        try:
            result = action()
            return result
        except Exception:
            self.renderer.release_offscreen()
            raise
        finally:
            self._busy = False
            self._lock.release()

    def submit_render(
        self,
        *,
        step: float | None = None,
        view_matrix: Any = None,
        error_threshold: float | None = None,
    ) -> RenderResult:
        def action() -> RenderResult:
            result = self.renderer.render(
                step=step,
                view_matrix=view_matrix,
                error_threshold=error_threshold,
            )
            self.last_good_result = result
            return result

        return self._submit(action)

    def submit_window_and_render(
        self,
        width: float,
        level: float,
    ) -> tuple[MappingChange, RenderResult]:
        def action() -> tuple[MappingChange, RenderResult]:
            change = self.renderer.set_window(width, level)
            result = self.renderer.render()
            self.last_good_result = result
            return change, result

        return self._submit(action)

    def submit_transfer_and_render(
        self,
        points: Any,
    ) -> RenderResult:
        def action() -> RenderResult:
            self.renderer.set_transfer_points(points)
            result = self.renderer.render()
            self.last_good_result = result
            return result

        return self._submit(action)

    def submit_precision_switch(self, mode: str) -> ModeSwitchReport:
        def action() -> ModeSwitchReport:
            report = self.renderer.set_precision(mode)
            self.last_good_result = report.result
            return report

        return self._submit(action)

    def submit_compare_precision(self) -> PrecisionComparison:
        return self._submit(self.renderer.compare_precision)

    def submit_lighting_audit(
        self,
        *,
        tolerance: float | None = None,
        fine_step: float | None = None,
    ) -> LightDriftReport:
        return self._submit(
            lambda: self.renderer.audit_lighting_drift(
                tolerance=tolerance,
                fine_step=fine_step,
            )
        )

    def submit_restore_snapshot(self, snapshot: Any, *, rerender: bool = False) -> Any:
        def action() -> Any:
            result = self.renderer.restore_snapshot(snapshot, rerender=rerender)
            self.last_good_result = self.renderer.last_good_result
            return result

        return self._submit(action)

    def submit_snapshot_save(
        self,
        path: str | Path,
        *,
        expected_state_hash: str | None = None,
    ) -> Path:
        return self._submit(
            lambda: self.renderer.save_snapshot(
                path,
                result=self.last_good_result,
                expected_state_hash=expected_state_hash,
            )
        )
