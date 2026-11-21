"""Preview state machine that binds every visible image to committed parameters."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .renderer import RenderResult


@dataclass(frozen=True)
class PreviewFrame:
    params: RenderParameters
    result: RenderResult

    @property
    def parameter_hash(self) -> str:
        return self.params.parameter_hash


def validate_preview_binding(params: RenderParameters, result: RenderResult) -> None:
    if result.parameter_hash != params.parameter_hash:
        fields = params.describe_differences(result.parameters) if result.parameters is not None else []
        raise VolumeRenderError(
            ErrorCode.E_PREVIEW_MISMATCH,
            "preview parameter hash differs from the drawn buffer binding",
            field=(fields[0] if fields else "parameter_hash"),
            details={
                "displayed_parameter_hash": params.parameter_hash,
                "buffer_parameter_hash": result.parameter_hash,
                "differing_fields": fields,
            },
        )
    if result.parameters is not None and result.parameters.parameter_hash != result.parameter_hash:
        fields = params.describe_differences(result.parameters)
        raise VolumeRenderError(
            ErrorCode.E_PREVIEW_MISMATCH,
            "buffer metadata is internally inconsistent",
            field=(fields[0] if fields else "parameters.parameter_hash"),
        )
    if params.viewport != (result.width, result.height):
        raise VolumeRenderError(
            ErrorCode.E_PREVIEW_MISMATCH,
            "preview viewport differs from the drawn buffer size",
            field="viewport",
            details={"expected": list(params.viewport), "actual": [result.width, result.height]},
        )


class PreviewController:
    """A display can only change through :meth:`commit`.

    Slider movement updates only ``pending``.  Consequently the currently
    displayed pixels, numeric readout and hash always remain from one committed
    parameter group until a new offscreen result is accepted.
    """

    def __init__(self) -> None:
        self._frame: PreviewFrame | None = None
        self._pending: RenderParameters | None = None

    @property
    def frame(self) -> PreviewFrame | None:
        return self._frame

    @property
    def pending(self) -> RenderParameters | None:
        return self._pending

    @property
    def displayed_params(self) -> RenderParameters | None:
        return self._frame.params if self._frame is not None else None

    @property
    def displayed_parameter_hash(self) -> str | None:
        return self._frame.params.parameter_hash if self._frame is not None else None

    @property
    def displayed_buffer_hash(self) -> str | None:
        return self._frame.result.buffer_hash if self._frame is not None else None

    def set_pending(self, pending: RenderParameters | None) -> None:
        self._pending = pending

    def clear_pending(self) -> None:
        self._pending = None

    def commit(self, params: RenderParameters, result: RenderResult) -> PreviewFrame:
        validate_preview_binding(params, result)
        frame = PreviewFrame(params=params, result=result)
        self._frame = frame
        self._pending = None
        return frame

    def validate(self) -> None:
        if self._frame is None:
            return
        validate_preview_binding(self._frame.params, self._frame.result)

    def state(self) -> dict[str, Any]:
        displayed = self._frame.params if self._frame is not None else None
        result = self._frame.result if self._frame is not None else None
        pending = self._pending
        return {
            "displayed_parameter_hash": displayed.parameter_hash if displayed is not None else None,
            "displayed_buffer_hash": result.buffer_hash if result is not None else None,
            "displayed_window_center": displayed.window_center if displayed is not None else None,
            "displayed_window_width": displayed.window_width if displayed is not None else None,
            "displayed_control_points": (
                [point.to_dict() for point in displayed.control_points] if displayed is not None else None
            ),
            "displayed_step_size": displayed.step_size if displayed is not None else None,
            "displayed_viewport": list(displayed.viewport) if displayed is not None else None,
            "displayed_mask_enabled": displayed.mask_enabled if displayed is not None else None,
            "pending_parameter_hash": pending.parameter_hash if pending is not None else None,
            "pending_window_center": pending.window_center if pending is not None else None,
            "pending_window_width": pending.window_width if pending is not None else None,
            "pending_control_points": (
                [point.to_dict() for point in pending.control_points] if pending is not None else None
            ),
            "pending_step_size": pending.step_size if pending is not None else None,
            "pending_is_uncommitted": pending is not None,
            "binding_valid": (
                result is not None
                and displayed is not None
                and result.parameter_hash == displayed.parameter_hash
                and result.width == displayed.viewport[0]
                and result.height == displayed.viewport[1]
            ),
        }
