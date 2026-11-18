from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .scene import ReverbRegion


@dataclass(frozen=True)
class ReverbValues:
    rt60: float
    wet_dry: float
    region_index: int


class ReverbSchedule:
    def __init__(self, regions: list[ReverbRegion], blend_width: float) -> None:
        if not regions:
            raise ValueError("at least one reverb region is required")
        self.regions = regions
        self.blend_width = float(blend_width)
        self.starts = np.asarray(
            [region.start_time for region in regions], dtype=np.float64
        )

    def values_at(self, times: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        times = np.asarray(times, dtype=np.float64)
        indices = np.searchsorted(self.starts, times, side="right") - 1
        indices = np.clip(indices, 0, len(self.regions) - 1)
        rt60 = np.asarray(
            [self.regions[index].rt60 for index in indices], dtype=np.float64
        )
        wet_dry = np.asarray(
            [self.regions[index].wet_dry for index in indices], dtype=np.float64
        )
        half = 0.5 * self.blend_width
        for boundary_index in range(1, len(self.regions)):
            boundary = self.regions[boundary_index].start_time
            low = boundary - half
            high = boundary + half
            mask = (times >= low) & (times <= high)
            if not np.any(mask):
                continue
            u = (times[mask] - low) / self.blend_width
            smooth = u * u * (3.0 - 2.0 * u)
            previous = self.regions[boundary_index - 1]
            current = self.regions[boundary_index]
            rt60[mask] = previous.rt60 + smooth * (current.rt60 - previous.rt60)
            wet_dry[mask] = previous.wet_dry + smooth * (
                current.wet_dry - previous.wet_dry
            )
        return rt60, wet_dry, indices

    def state_at(self, time: float) -> ReverbValues:
        rt60, wet_dry, indices = self.values_at(
            np.asarray([time], dtype=np.float64)
        )
        return ReverbValues(
            rt60=float(rt60[0]),
            wet_dry=float(wet_dry[0]),
            region_index=int(indices[0]),
        )


@dataclass
class ReverbState:
    comb_positions: list[list[int]]
    comb_buffers: list[list[np.ndarray]]
    damp_states: list[list[float]]
    allpass_positions: list[list[int]]
    allpass_buffers: list[list[np.ndarray]]


class StereoReverb:
    LEFT_COMBS_S = (0.0297, 0.0371, 0.0411, 0.0437)
    RIGHT_COMBS_S = (0.0313, 0.0389, 0.0427, 0.0453)
    LEFT_ALLPASS_S = (0.0050, 0.0017)
    RIGHT_ALLPASS_S = (0.0053, 0.0019)
    ALLPASS_GAIN = 0.5
    DAMPING = 0.28

    def __init__(self, sample_rate: int, state: ReverbState | None = None) -> None:
        self.sample_rate = int(sample_rate)
        comb_seconds = (self.LEFT_COMBS_S, self.RIGHT_COMBS_S)
        allpass_seconds = (self.LEFT_ALLPASS_S, self.RIGHT_ALLPASS_S)
        self.comb_delays = [
            [max(1, int(round(seconds * sample_rate))) for seconds in channel]
            for channel in comb_seconds
        ]
        self.allpass_delays = [
            [max(1, int(round(seconds * sample_rate))) for seconds in channel]
            for channel in allpass_seconds
        ]
        if state is None:
            self.state = ReverbState(
                comb_positions=[[0] * 4, [0] * 4],
                comb_buffers=[
                    [np.zeros(delay, dtype=np.float64) for delay in self.comb_delays[0]],
                    [np.zeros(delay, dtype=np.float64) for delay in self.comb_delays[1]],
                ],
                damp_states=[[0.0] * 4, [0.0] * 4],
                allpass_positions=[[0] * 2, [0] * 2],
                allpass_buffers=[
                    [np.zeros(delay, dtype=np.float64) for delay in self.allpass_delays[0]],
                    [np.zeros(delay, dtype=np.float64) for delay in self.allpass_delays[1]],
                ],
            )
        else:
            self.state = state
            self._validate_state()

    def _validate_state(self) -> None:
        for channel in range(2):
            if len(self.state.comb_positions[channel]) != 4:
                raise ValueError("invalid reverb comb position state")
            if len(self.state.comb_buffers[channel]) != 4:
                raise ValueError("invalid reverb comb buffer state")
            for index, delay in enumerate(self.comb_delays[channel]):
                if self.state.comb_buffers[channel][index].shape != (delay,):
                    raise ValueError("invalid reverb comb buffer shape")
            if len(self.state.allpass_positions[channel]) != 2:
                raise ValueError("invalid reverb allpass position state")
            for index, delay in enumerate(self.allpass_delays[channel]):
                if self.state.allpass_buffers[channel][index].shape != (delay,):
                    raise ValueError("invalid reverb allpass buffer shape")

    def process(
        self,
        dry: np.ndarray,
        rt60: np.ndarray,
        wet_dry: np.ndarray,
    ) -> tuple[np.ndarray, np.ndarray]:
        if dry.ndim != 2 or dry.shape[1] != 2:
            raise ValueError("reverb input must be (frames, 2)")
        frame_count = dry.shape[0]
        if rt60.shape != (frame_count,) or wet_dry.shape != (frame_count,):
            raise ValueError("reverb parameter arrays must match input length")
        safe_rt = np.maximum(rt60, 0.05)
        comb_gains: list[list[np.ndarray]] = [[], []]
        for channel in range(2):
            for delay in self.comb_delays[channel]:
                delay_seconds = delay / self.sample_rate
                comb_gains[channel].append(
                    np.power(10.0, -3.0 * delay_seconds / safe_rt)
                )

        wet = np.zeros_like(dry, dtype=np.float64)
        damping = self.DAMPING
        for frame in range(frame_count):
            for channel in range(2):
                input_sample = float(dry[frame, channel]) * 0.55
                comb_sum = 0.0
                for comb in range(4):
                    delay = self.comb_delays[channel][comb]
                    position = self.state.comb_positions[channel][comb]
                    delayed = float(
                        self.state.comb_buffers[channel][comb][position]
                    )
                    damped = (
                        (1.0 - damping) * delayed
                        + damping * self.state.damp_states[channel][comb]
                    )
                    self.state.damp_states[channel][comb] = damped
                    self.state.comb_buffers[channel][comb][position] = (
                        input_sample + damped * comb_gains[channel][comb][frame]
                    )
                    self.state.comb_positions[channel][comb] = (position + 1) % delay
                    comb_sum += delayed
                signal = comb_sum / 4.0
                for allpass in range(2):
                    delay = self.allpass_delays[channel][allpass]
                    position = self.state.allpass_positions[channel][allpass]
                    delayed = float(
                        self.state.allpass_buffers[channel][allpass][position]
                    )
                    output = -self.ALLPASS_GAIN * signal + delayed
                    self.state.allpass_buffers[channel][allpass][position] = (
                        signal + self.ALLPASS_GAIN * output
                    )
                    self.state.allpass_positions[channel][allpass] = (
                        position + 1
                    ) % delay
                    signal = output
                wet[frame, channel] = signal
        return wet, wet_dry

    def export_state(self) -> dict[str, np.ndarray]:
        result: dict[str, np.ndarray] = {}
        for channel in range(2):
            for comb in range(4):
                result[f"comb_pos_{channel}_{comb}"] = np.asarray(
                    [self.state.comb_positions[channel][comb]], dtype=np.int64
                )
                result[f"comb_buf_{channel}_{comb}"] = np.asarray(
                    self.state.comb_buffers[channel][comb], dtype=np.float64
                )
                result[f"damp_{channel}_{comb}"] = np.asarray(
                    [self.state.damp_states[channel][comb]], dtype=np.float64
                )
            for allpass in range(2):
                result[f"ap_pos_{channel}_{allpass}"] = np.asarray(
                    [self.state.allpass_positions[channel][allpass]], dtype=np.int64
                )
                result[f"ap_buf_{channel}_{allpass}"] = np.asarray(
                    self.state.allpass_buffers[channel][allpass], dtype=np.float64
                )
        return result

    @classmethod
    def from_exported(
        cls, sample_rate: int, arrays: dict[str, Any]
    ) -> "StereoReverb":
        comb_positions: list[list[int]] = [[], []]
        comb_buffers: list[list[np.ndarray]] = [[], []]
        damp_states: list[list[float]] = [[], []]
        allpass_positions: list[list[int]] = [[], []]
        allpass_buffers: list[list[np.ndarray]] = [[], []]
        for channel in range(2):
            for comb in range(4):
                comb_positions[channel].append(
                    int(np.asarray(arrays[f"comb_pos_{channel}_{comb}"]).reshape(-1)[0])
                )
                comb_buffers[channel].append(
                    np.asarray(
                        arrays[f"comb_buf_{channel}_{comb}"], dtype=np.float64
                    ).copy()
                )
                damp_states[channel].append(
                    float(np.asarray(arrays[f"damp_{channel}_{comb}"]).reshape(-1)[0])
                )
            for allpass in range(2):
                allpass_positions[channel].append(
                    int(
                        np.asarray(arrays[f"ap_pos_{channel}_{allpass}"]).reshape(-1)[0]
                    )
                )
                allpass_buffers[channel].append(
                    np.asarray(
                        arrays[f"ap_buf_{channel}_{allpass}"], dtype=np.float64
                    ).copy()
                )
        return cls(
            sample_rate,
            ReverbState(
                comb_positions=comb_positions,
                comb_buffers=comb_buffers,
                damp_states=damp_states,
                allpass_positions=allpass_positions,
                allpass_buffers=allpass_buffers,
            ),
        )

