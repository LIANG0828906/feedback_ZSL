from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .scene import CameraSample


@dataclass(frozen=True)
class CameraFrame:
    position: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    up: np.ndarray


class CameraPath:
    def __init__(self, samples: list[CameraSample], world_up: np.ndarray) -> None:
        self.samples = samples
        self.times = np.asarray([sample.time for sample in samples], dtype=np.float64)
        self.positions = np.asarray(
            [sample.position for sample in samples], dtype=np.float64
        )
        self.forwards = np.asarray(
            [sample.forward for sample in samples], dtype=np.float64
        )
        self.world_up = np.asarray(world_up, dtype=np.float64)

    def _fractional_index(self, times: np.ndarray) -> np.ndarray:
        return np.interp(
            times,
            self.times,
            np.arange(self.times.shape[0], dtype=np.float64),
        )

    def position_at(self, times: np.ndarray) -> np.ndarray:
        return np.column_stack(
            [
                np.interp(times, self.times, self.positions[:, axis])
                for axis in range(3)
            ]
        )

    def forward_at(self, times: np.ndarray) -> np.ndarray:
        forward = np.column_stack(
            [
                np.interp(times, self.times, self.forwards[:, axis])
                for axis in range(3)
            ]
        )
        norms = np.linalg.norm(forward, axis=1)
        norms = np.maximum(norms, 1e-12)
        return forward / norms[:, None]

    def frames_at(self, times: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        positions = self.position_at(times)
        forwards = self.forward_at(times)
        rights = np.cross(forwards, self.world_up[None, :])
        right_norms = np.linalg.norm(rights, axis=1)
        fallback = np.array([1.0, 0.0, 0.0], dtype=np.float64)
        bad = right_norms < 1e-9
        if np.any(bad):
            rights[bad] = fallback
            right_norms[bad] = 1.0
        rights = rights / right_norms[:, None]
        ups = np.cross(rights, forwards)
        ups = ups / np.maximum(np.linalg.norm(ups, axis=1), 1e-12)[:, None]
        return positions, np.column_stack([forwards, rights, ups])

    def frame_at(self, time: float) -> CameraFrame:
        positions, frames = self.frames_at(np.asarray([time], dtype=np.float64))
        return CameraFrame(
            position=positions[0],
            forward=frames[0, 0:3],
            right=frames[0, 3:6],
            up=frames[0, 6:9],
        )

