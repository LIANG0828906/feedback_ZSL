"""Input-level fault injection.

Faults are deliberately applied to a copy of the loaded input, so the normal
sample and the anomaly path are reproducible without editing source code.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import ErrorCode, WorkbenchError
from .models import AcousticSample


@dataclass(frozen=True)
class FaultPlan:
    missing_channel: str | int | None = None
    reverse_time: bool = False
    gain: float = 1.0
    bad_frames: tuple[int, ...] = ()

    @classmethod
    def from_dict(cls, value: dict[str, Any] | None) -> "FaultPlan":
        if not value:
            return cls()
        raw_bad_frames = value.get("bad_frames", ())
        try:
            bad_frames = tuple(int(item) for item in raw_bad_frames)
        except (TypeError, ValueError) as exc:
            raise WorkbenchError(
                ErrorCode.E_FRAME,
                "bad_frames fault injection requires integer frame indices",
                stage="fault-injection",
            ) from exc
        try:
            gain = float(value.get("gain", 1.0))
        except (TypeError, ValueError) as exc:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "gain fault injection must be numeric",
                stage="fault-injection",
            ) from exc
        if not np.isfinite(gain) or gain <= 0.0:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "gain fault injection must be positive and finite",
                stage="fault-injection",
            )
        return cls(
            missing_channel=value.get("missing_channel"),
            reverse_time=bool(value.get("reverse_time", False)),
            gain=gain,
            bad_frames=bad_frames,
        )

    def apply(self, sample: AcousticSample) -> AcousticSample:
        ir = np.array(sample.ir, copy=True)
        dry = np.array(sample.dry, copy=True)
        missing = tuple(sample.missing_channels)
        if self.missing_channel is not None:
            if isinstance(self.missing_channel, int):
                try:
                    channel_name = sample.layout.channels[self.missing_channel]
                except IndexError as exc:
                    raise WorkbenchError(
                        ErrorCode.E_CHAN,
                        "missing_channel index is outside the layout",
                        stage="fault-injection",
                        channel_index=int(self.missing_channel),
                    ) from exc
            else:
                channel_name = str(self.missing_channel)
            if channel_name not in sample.layout.channels:
                raise WorkbenchError(
                    ErrorCode.E_CHAN,
                    "missing_channel is not present in the layout",
                    stage="fault-injection",
                    details={"missing_channel": channel_name},
                )
            missing = tuple(sorted(set((*missing, channel_name))))
        if self.reverse_time:
            ir = np.ascontiguousarray(ir[:, ::-1])
        if self.gain != 1.0:
            ir = np.asarray(ir * self.gain, dtype=np.float64)
            dry = np.asarray(dry * self.gain, dtype=np.float64)
        bad_frames = tuple(sorted(set((*sample.bad_frames, *self.bad_frames))))
        return sample.copy_with(
            ir=ir,
            dry=dry,
            bad_frames=bad_frames,
            missing_channels=missing,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "missing_channel": self.missing_channel,
            "reverse_time": self.reverse_time,
            "gain": self.gain,
            "bad_frames": list(self.bad_frames),
        }

