"""Business error codes shared by the core, CLI and GUI."""

from __future__ import annotations

from typing import Any, Dict, Optional


E_IMG_INVALID = "E_IMG_INVALID"
E_MODE_INVALID = "E_MODE_INVALID"
E_BUSY = "E_BUSY"
E_ERROR_ATTR = "E_ERROR_ATTR"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_FACTOR_INVALID = "E_FACTOR_INVALID"
E_CANCELLED = "E_CANCELLED"
E_TRACE_INVALID = "E_TRACE_INVALID"


class WorkbenchError(Exception):
    """A deterministic, serialisable business error.

    ``frame`` and ``block`` identify the first anomalous location whenever that
    location is known.  Pixel coordinates use zero-based ``(y, x)``.
    """

    def __init__(
        self,
        code: str,
        message: str,
        *,
        frame: Optional[int] = None,
        block: Optional[int] = None,
        pixel: Optional[Any] = None,
        path: Optional[str] = None,
        details: Optional[Dict[str, Any]] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.frame = frame
        self.block = block
        self.pixel = tuple(pixel) if pixel is not None else None
        self.path = path
        self.details = details or {}

    @property
    def frame_index(self) -> Optional[int]:
        return self.frame

    @property
    def block_index(self) -> Optional[int]:
        return self.block

    @property
    def pixel_index(self) -> Optional[Any]:
        return self.pixel

    @property
    def first_bad_frame(self) -> Optional[int]:
        return self.frame

    @property
    def first_bad_block(self) -> Optional[int]:
        return self.block

    def to_dict(self) -> Dict[str, Any]:
        payload: Dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        if self.frame is not None:
            payload["first_bad_frame"] = int(self.frame)
        if self.block is not None:
            payload["first_bad_block"] = int(self.block)
        if self.pixel is not None:
            payload["first_bad_pixel"] = [int(self.pixel[0]), int(self.pixel[1])]
        if self.path is not None:
            payload["path"] = self.path
        if self.details:
            payload["details"] = self.details
        return payload

    def __str__(self) -> str:
        location = []
        if self.frame is not None:
            location.append("frame={}".format(self.frame))
        if self.block is not None:
            location.append("block={}".format(self.block))
        if self.pixel is not None:
            location.append("pixel=({},{})".format(self.pixel[0], self.pixel[1]))
        suffix = " [" + ", ".join(location) + "]" if location else ""
        return "{}: {}{}".format(self.code, self.message, suffix)


class ForcedTermination(BaseException):
    """Raised by the test hook that simulates an abrupt process termination."""

    def __init__(self, committed_blocks: int) -> None:
        super().__init__(
            "simulated forced termination after {} committed blocks".format(
                committed_blocks
            )
        )
        self.committed_blocks = committed_blocks
