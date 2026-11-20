"""Streaming observation-block meter and offline consistency checks."""

from __future__ import annotations

import math
import threading
from dataclasses import dataclass, replace
from typing import Any, Sequence

import numpy as np

from .budget import (
    BudgetPlan,
    enforce_or_raise,
    initial_plan,
)
from .config import MeterConfig
from .errors import ErrorCode, MeterError, error
from .models import (
    DBTP_FLOOR,
    LOUDNESS_FLOOR,
    AssetReport,
    BlockResult,
    ResliceDiff,
    build_report,
)
from .wav import ChannelLayout, DownmixResult, WavInfo, WaveAsset, downmix


# ITU-R BS.1770 K-weighting coefficients at 48 kHz.
K_SHELF_B = (1.53512485958697, -2.69169618940638, 1.19839281085285)
K_SHELF_A = (1.0, -1.69065929318241, 0.73248077421585)
K_HIGHPASS_B = (1.0, -2.0, 1.0)
K_HIGHPASS_A = (1.0, -1.99004745483398, 0.99007225036621)


def loudness_from_energy(energy: float) -> float:
    if not np.isfinite(energy) or energy <= 0.0:
        return float("-inf")
    return -0.691 + 10.0 * math.log10(float(energy))


def _published_loudness(value: float) -> float:
    return max(LOUDNESS_FLOOR, float(value))


def _dbtp(peak: float) -> float:
    if not np.isfinite(peak) or peak <= 0.0:
        return DBTP_FLOOR
    return max(DBTP_FLOOR, 20.0 * math.log10(float(peak)))


class _BiquadStage:
    def __init__(self, b: Sequence[float], a: Sequence[float], channels: int):
        self.b0, self.b1, self.b2 = (float(x) for x in b)
        self.a1, self.a2 = float(a[1]), float(a[2])
        self.z1 = np.zeros(channels, dtype=np.float64)
        self.z2 = np.zeros(channels, dtype=np.float64)

    def reset(self) -> None:
        self.z1.fill(0.0)
        self.z2.fill(0.0)

    def process(self, samples: np.ndarray) -> np.ndarray:
        x = np.asarray(samples, dtype=np.float64)
        out = np.empty_like(x)
        z1 = self.z1
        z2 = self.z2
        b0, b1, b2 = self.b0, self.b1, self.b2
        a1, a2 = self.a1, self.a2
        for index in range(x.shape[0]):
            xn = x[index]
            yn = b0 * xn + z1
            z1 = b1 * xn - a1 * yn + z2
            z2 = b2 * xn - a2 * yn
            out[index] = yn
        self.z1 = z1
        self.z2 = z2
        return out


class KWeightingBank:
    def __init__(self, channels: int):
        self.channels = channels
        self.shelf = _BiquadStage(K_SHELF_B, K_SHELF_A, channels)
        self.highpass = _BiquadStage(K_HIGHPASS_B, K_HIGHPASS_A, channels)

    def reset(self) -> None:
        self.shelf.reset()
        self.highpass.reset()

    def process(self, samples: np.ndarray) -> np.ndarray:
        return self.highpass.process(self.shelf.process(samples))


class _FilteredHistory:
    """Ring buffer for at most the short-term window."""

    def __init__(self, capacity: int, channels: int):
        self.capacity = max(1, int(capacity))
        self.channels = int(channels)
        self.data = np.zeros((self.capacity, self.channels), dtype=np.float64)
        self.write_index = 0
        self.count = 0

    def reset(self) -> None:
        self.data.fill(0.0)
        self.write_index = 0
        self.count = 0

    def append(self, values: np.ndarray) -> None:
        values = np.asarray(values, dtype=np.float64)
        length = values.shape[0]
        if length == 0:
            return
        if length >= self.capacity:
            self.data[:] = values[-self.capacity :]
            self.write_index = 0
            self.count = self.capacity
            return
        first = min(length, self.capacity - self.write_index)
        self.data[self.write_index : self.write_index + first] = values[:first]
        remaining = length - first
        if remaining:
            self.data[:remaining] = values[first:]
        self.write_index = (self.write_index + length) % self.capacity
        self.count = min(self.capacity, self.count + length)

    def tail(self, length: int) -> np.ndarray:
        length = min(max(0, int(length)), self.count)
        if length == 0:
            return np.empty((0, self.channels), dtype=np.float64)
        start = (self.write_index - length) % self.capacity
        end = start + length
        if end <= self.capacity:
            return self.data[start:end]
        return np.concatenate((self.data[start:], self.data[: end - self.capacity]))

    def mean_square(self, length: int) -> np.ndarray:
        values = self.tail(length)
        if values.size == 0:
            return np.zeros(self.channels, dtype=np.float64)
        return np.mean(values * values, axis=0, dtype=np.float64)


def _oversampling_kernel(factor: int) -> np.ndarray:
    if factor <= 1:
        return np.ones(1, dtype=np.float64)
    taps = factor * 16 + 1
    half = (taps - 1) // 2
    positions = np.arange(-half, half + 1, dtype=np.float64)
    kernel = np.sinc(positions / factor) / factor
    kernel *= np.hamming(taps)
    return kernel


def true_peak_estimate(
    signals: np.ndarray,
    *,
    oversample_factor: int,
) -> tuple[float, float]:
    """Return (4x true-peak estimate, sample peak) as linear magnitudes."""
    signals = np.asarray(signals, dtype=np.float64)
    if signals.size == 0:
        return 0.0, 0.0
    sample_peak = float(np.max(np.abs(signals)))
    if oversample_factor <= 1:
        return sample_peak, sample_peak
    kernel = _oversampling_kernel(oversample_factor)
    frames = signals.shape[0]
    upsampled = np.zeros(
        (frames * oversample_factor, signals.shape[1]),
        dtype=np.float64,
    )
    upsampled[::oversample_factor] = signals
    peak = 0.0
    for channel in range(signals.shape[1]):
        filtered = np.convolve(upsampled[:, channel], kernel, mode="same")
        if filtered.size:
            peak = max(peak, float(np.max(np.abs(filtered))))
    return max(peak, sample_peak), sample_peak


@dataclass(frozen=True)
class RawBlock:
    block_index: int
    window_start: int
    window_end: int
    sync_marker: str
    content_hash: str
    short_term_lufs: float
    short_term_energy: float
    block_energy: float
    true_peak_dbtp: float
    sample_peak_dbtp: float
    degrade_stage: str
    memory_bytes: int
    partial: bool


@dataclass(frozen=True)
class UpdateResult:
    report: AssetReport
    affected_raw_blocks: tuple[int, ...]
    affected_derived_blocks: tuple[int, ...]
    reused_raw_blocks: tuple[int, ...]
    reslice: ResliceDiff | None
    parameter_version: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "parameter_version": self.parameter_version,
            "affected_raw_blocks": list(self.affected_raw_blocks),
            "affected_derived_blocks": list(self.affected_derived_blocks),
            "reused_raw_blocks": list(self.reused_raw_blocks),
            "reslice": self.reslice.to_dict() if self.reslice else None,
        }


def _boundaries(total_samples: int, block_samples: int) -> tuple[int, ...]:
    if total_samples <= 0:
        return (0,)
    return tuple(range(0, total_samples, block_samples)) + (total_samples,)


def reslice_diff(
    total_samples: int,
    old_block_samples: int,
    new_block_samples: int,
) -> ResliceDiff:
    old = _boundaries(total_samples, old_block_samples)
    new = _boundaries(total_samples, new_block_samples)
    old_set = set(old)
    new_set = set(new)
    old_blocks = [
        (old[index], old[index + 1]) for index in range(max(0, len(old) - 1))
    ]
    new_blocks = [
        (new[index], new[index + 1]) for index in range(max(0, len(new) - 1))
    ]
    changed: list[int] = []
    for index in range(max(len(old_blocks), len(new_blocks))):
        if index >= len(old_blocks) or index >= len(new_blocks):
            changed.append(index)
        elif old_blocks[index] != new_blocks[index]:
            changed.append(index)
    common = sorted(old_set & new_set)
    old_index = {value: index for index, value in enumerate(old)}
    new_index = {value: index for index, value in enumerate(new)}
    max_shift = max(
        (
            abs(old_index[value] * old_block_samples - new_index[value] * new_block_samples)
            for value in common
        ),
        default=0,
    )
    return ResliceDiff(
        old_block_samples=old_block_samples,
        new_block_samples=new_block_samples,
        old_boundaries=old,
        new_boundaries=new,
        added_boundaries=tuple(sorted(new_set - old_set)),
        removed_boundaries=tuple(sorted(old_set - new_set)),
        changed_blocks=tuple(changed),
        max_boundary_shift_samples=int(max_shift),
    )


class AssetMeter:
    """One asset's stateful observation-block meter.

    Only a single thread may advance one instance at a time.  Different assets
    may be advanced in parallel by the scheduler.  Published reports are
    replaced as one immutable object, so readers always see a complete version.
    """

    FILTER_VERSION = "bs1770-kweighting-v1"

    def __init__(
        self,
        path: str,
        *,
        config: MeterConfig,
        asset_id: str | None = None,
        enforce_budget: bool = True,
    ):
        config.validate()
        self.config = config
        self.asset = WaveAsset(path)
        self.info: WavInfo = self.asset.info
        self.asset_id = asset_id or self.info.path.rsplit("/", 1)[-1]
        self.enforce_budget = enforce_budget
        self._lock = threading.RLock()
        self._parameter_version = int(config.version)
        self._next_state_index = 0
        self._raw_blocks: dict[int, RawBlock] = {}
        self._current_samples: np.ndarray | None = None
        self._last_budget_events: list[dict[str, Any]] = []
        self._last_budget_plan = initial_plan(config.scheduler)
        self._weights = self._resolve_weights()
        self._measurement_signature = self._signature()
        self._kfilter = KWeightingBank(self.info.channels)
        self._history = _FilteredHistory(
            config.short_term_samples,
            self.info.channels,
        )
        self._published = self._build_current_report()

    def _resolve_weights(self) -> tuple[float, ...]:
        if self.config.channel_weights is not None:
            weights = tuple(float(x) for x in self.config.channel_weights)
        else:
            weights = ChannelLayout(
                self.info.layout,
                self.info.channel_mask,
                self.info.labels,
            ).weights
        if len(weights) != self.info.channels:
            raise error(
                ErrorCode.E_CHANNEL_UNSUPPORTED,
                "channel weight count does not match layout",
                {
                    "channels": self.info.channels,
                    "layout": self.info.layout,
                    "weights": list(weights),
                },
            )
        return weights

    def _signature(self) -> tuple[Any, ...]:
        return (
            self.FILTER_VERSION,
            self.config.sample_rate,
            self.config.block_samples,
            self.config.short_term_samples,
            self.config.oversample_factor,
            self.config.true_peak_include_lfe,
            self._weights,
        )

    def _derived_signature(self) -> tuple[Any, ...]:
        return (
            self.config.target_lufs,
            self.config.true_peak_limit_dbtp,
            self.config.absolute_gate_lufs,
            self.config.relative_gate_lu,
        )

    @property
    def total_blocks(self) -> int:
        if self.info.num_frames == 0:
            return 0
        return math.ceil(self.info.num_frames / self.config.block_samples)

    @property
    def parameter_version(self) -> int:
        return self._parameter_version

    def _current_config_hash(self) -> str:
        return self.config.config_hash

    def _block_range(self, index: int) -> tuple[int, int]:
        if index < 0 or index >= self.total_blocks:
            raise error(
                ErrorCode.E_BLOCK_NOT_READY,
                "observation block index is out of range",
                {
                    "asset_id": self.asset_id,
                    "block_index": index,
                    "block_count": self.total_blocks,
                },
            )
        start = index * self.config.block_samples
        end = min(self.info.num_frames, start + self.config.block_samples)
        return start, end

    def _budget_for(self, index: int) -> tuple[BudgetPlan, int]:
        start, end = self._block_range(index)
        retained = min(self.config.short_term_samples, end)
        if self.enforce_budget:
            plan, events, estimated = enforce_or_raise(
                self.config.scheduler,
                asset_id=self.asset_id,
                block_index=index,
                block_samples=end - start,
                channels=self.info.channels,
                retained_samples=retained,
                oversample_factor=self.config.oversample_factor,
            )
            self._last_budget_events = events
            self._last_budget_plan = plan
        else:
            plan = initial_plan(self.config.scheduler)
            estimated = plan.estimate_memory_bytes(
                block_samples=end - start,
                channels=self.info.channels,
                retained_samples=retained,
                oversample_factor=self.config.oversample_factor,
            )
            self._last_budget_events = []
            self._last_budget_plan = plan
        return plan, estimated

    def _compute_raw(
        self,
        index: int,
        *,
        plan: BudgetPlan,
        memory_bytes: int,
    ) -> RawBlock:
        start, end = self._block_range(index)
        samples = self.asset.read_frames(start, end)
        self._current_samples = samples
        mixed: DownmixResult = downmix(
            samples,
            self.info,
            channel_weights=self._weights,
            true_peak_include_lfe=self.config.true_peak_include_lfe,
        )
        filtered = self._kfilter.process(mixed.signals)
        self._history.append(filtered)
        short_frames = min(
            self.config.short_term_samples,
            end - max(0, end - self.config.short_term_samples),
        )
        short_energy = float(
            np.sum(self._history.mean_square(short_frames), dtype=np.float64)
        )
        if filtered.shape[0]:
            block_energy = float(
                np.mean(np.sum(filtered * filtered, axis=1), dtype=np.float64)
            )
        else:
            block_energy = 0.0
        tp_linear, sample_peak_linear = true_peak_estimate(
            mixed.true_peak_signals,
            oversample_factor=self.config.oversample_factor,
        )
        content_hash = self.asset.content_hash(start, end)
        return RawBlock(
            block_index=index,
            window_start=start,
            window_end=end,
            sync_marker=f"{self.asset_id}:{index:06d}",
            content_hash=content_hash,
            short_term_lufs=_published_loudness(loudness_from_energy(short_energy)),
            short_term_energy=short_energy,
            block_energy=block_energy,
            true_peak_dbtp=_dbtp(tp_linear),
            sample_peak_dbtp=_dbtp(sample_peak_linear),
            degrade_stage=plan.degrade_stage,
            memory_bytes=int(memory_bytes),
            partial=end - start < self.config.block_samples,
        )

    def _reset_processing(self) -> None:
        self._next_state_index = 0
        self._current_samples = None
        self._kfilter.reset()
        self._history.reset()

    def _ensure_through(
        self,
        target_index: int,
        *,
        final_plan: BudgetPlan | None = None,
        final_memory_bytes: int | None = None,
    ) -> None:
        """Advance filter state and cache through ``target_index``."""
        if target_index < 0 or target_index >= self.total_blocks:
            raise error(
                ErrorCode.E_BLOCK_NOT_READY,
                "requested block is outside the asset",
                {
                    "asset_id": self.asset_id,
                    "block_index": target_index,
                    "block_count": self.total_blocks,
                },
            )
        if target_index < self._next_state_index:
            if target_index not in self._raw_blocks:
                self.rebuild_to_block(target_index)
            return
        for index in range(self._next_state_index, target_index + 1):
            if index == target_index and final_plan is not None:
                plan = final_plan
                if final_memory_bytes is None:
                    start, end = self._block_range(index)
                    memory_bytes = plan.estimate_memory_bytes(
                        block_samples=end - start,
                        channels=self.info.channels,
                        retained_samples=min(self.config.short_term_samples, end),
                        oversample_factor=self.config.oversample_factor,
                    )
                else:
                    memory_bytes = final_memory_bytes
            else:
                plan, memory_bytes = self._budget_for(index)
            self._raw_blocks[index] = self._compute_raw(
                index,
                plan=plan,
                memory_bytes=memory_bytes,
            )
            self._next_state_index = index + 1

    def _integrated_prefix(
        self,
        raw_blocks: list[RawBlock],
    ) -> tuple[list[float], list[str]]:
        energies: list[float] = []
        values: list[float] = []
        statuses: list[str] = []
        for raw in raw_blocks:
            energies.append(raw.block_energy)
            absolute_active = [
                energy
                for energy in energies
                if loudness_from_energy(energy) >= self.config.absolute_gate_lufs
            ]
            if not absolute_active:
                values.append(LOUDNESS_FLOOR)
                statuses.append("silent")
                continue
            mean_absolute = float(np.mean(absolute_active, dtype=np.float64))
            relative_threshold = (
                loudness_from_energy(mean_absolute) + self.config.relative_gate_lu
            )
            relative_active = [
                energy
                for energy in absolute_active
                if loudness_from_energy(energy) >= relative_threshold
            ]
            if not relative_active:
                values.append(LOUDNESS_FLOOR)
                statuses.append("relative")
                continue
            integrated = loudness_from_energy(
                float(np.mean(relative_active, dtype=np.float64))
            )
            values.append(_published_loudness(integrated))
            statuses.append(
                "absolute"
                if len(relative_active) == len(absolute_active)
                else "relative"
            )
        return values, statuses

    def _build_current_report(self) -> AssetReport:
        available_count = 0
        while available_count in self._raw_blocks:
            available_count += 1
        raw_blocks = [self._raw_blocks[index] for index in range(available_count)]
        integrated, gate_status = self._integrated_prefix(raw_blocks)
        blocks: list[BlockResult] = []
        for index, raw in enumerate(raw_blocks):
            gain = self.config.target_lufs - raw.short_term_lufs
            blocks.append(
                BlockResult(
                    asset_id=self.asset_id,
                    block_index=raw.block_index,
                    window_start=raw.window_start,
                    window_end=raw.window_end,
                    sync_marker=raw.sync_marker,
                    content_hash=raw.content_hash,
                    short_term_lufs=raw.short_term_lufs,
                    integrated_lufs=integrated[index],
                    true_peak_dbtp=raw.true_peak_dbtp,
                    sample_peak_dbtp=raw.sample_peak_dbtp,
                    target_lufs=self.config.target_lufs,
                    true_peak_limit_dbtp=self.config.true_peak_limit_dbtp,
                    gain_to_target_db=float(gain),
                    true_peak_ok=raw.true_peak_dbtp <= self.config.true_peak_limit_dbtp,
                    gate_status=gate_status[index],
                    absolute_gate_lufs=self.config.absolute_gate_lufs,
                    relative_gate_lu=self.config.relative_gate_lu,
                    degrade_stage=raw.degrade_stage,
                    memory_bytes=raw.memory_bytes,
                    parameter_version=self._parameter_version,
                    partial=raw.partial,
                    block_energy=raw.block_energy,
                    short_term_energy=raw.short_term_energy,
                )
            )
        complete = self.total_blocks > 0 and available_count == self.total_blocks
        return build_report(
            asset_id=self.asset_id,
            sample_rate=self.info.sample_rate,
            channels=self.info.channels,
            bits_per_sample=self.info.bits_per_sample,
            layout=self.info.layout,
            channel_mask=self.info.channel_mask,
            block_samples=self.config.block_samples,
            total_samples=self.info.num_frames,
            parameter_version=self._parameter_version,
            config_hash=self._current_config_hash(),
            complete=complete,
            blocks=blocks,
        )

    def _publish(self) -> AssetReport:
        report = self._build_current_report()
        self._published = report
        return report

    @property
    def published(self) -> AssetReport:
        return self._published

    def get_published(self) -> AssetReport:
        # Reports and their blocks are frozen dataclasses, so returning the
        # current reference is already an atomic complete-version read.
        return self._published

    def latest_published(self) -> AssetReport:
        return self._published

    def process_block(
        self,
        index: int,
        *,
        budget_plan: BudgetPlan | None = None,
        memory_bytes: int | None = None,
    ) -> BlockResult:
        with self._lock:
            if index < self._next_state_index and index in self._raw_blocks:
                if index >= len(self._published.blocks):
                    self._publish()
                return self._published.blocks[index]
            if budget_plan is None:
                budget_plan, memory_bytes = self._budget_for(index)
            elif memory_bytes is None:
                start, end = self._block_range(index)
                memory_bytes = budget_plan.estimate_memory_bytes(
                    block_samples=end - start,
                    channels=self.info.channels,
                    retained_samples=min(self.config.short_term_samples, end),
                    oversample_factor=self.config.oversample_factor,
                )
            self._ensure_through(
                index,
                final_plan=budget_plan,
                final_memory_bytes=memory_bytes,
            )
            if index in self._raw_blocks and index >= len(self._published.blocks):
                self._publish()
            if index >= len(self._published.blocks):
                raise error(
                    ErrorCode.E_BLOCK_NOT_READY,
                    "block was not published",
                    {"asset_id": self.asset_id, "block_index": index},
                )
            return self._published.blocks[index]

    def process_next(
        self,
        *,
        budget_plan: BudgetPlan | None = None,
        memory_bytes: int | None = None,
    ) -> BlockResult:
        with self._lock:
            if self._next_state_index >= self.total_blocks:
                raise error(
                    ErrorCode.E_BLOCK_NOT_READY,
                    "all observation blocks have already been processed",
                    {
                        "asset_id": self.asset_id,
                        "block_count": self.total_blocks,
                    },
                )
            return self.process_block(
                self._next_state_index,
                budget_plan=budget_plan,
                memory_bytes=memory_bytes,
            )

    def process_all(self) -> AssetReport:
        while self._next_state_index < self.total_blocks:
            self.process_next()
        return self._published

    def task_memory_estimate(self, index: int) -> int:
        start, end = self._block_range(index)
        plan = initial_plan(self.config.scheduler)
        return plan.estimate_memory_bytes(
            block_samples=end - start,
            channels=self.info.channels,
            retained_samples=min(self.config.short_term_samples, end),
            oversample_factor=self.config.oversample_factor,
        )

    def rebuild_to_block(self, index: int) -> BlockResult:
        """Rebuild filter/history state from the start and publish through index."""
        with self._lock:
            for block_index in [
                key for key in self._raw_blocks if key > index
            ]:
                del self._raw_blocks[block_index]
            self._reset_processing()
            self._ensure_through(index)
            self._publish()
            return self._published.blocks[index]

    def seek_to_block(self, index: int) -> BlockResult:
        """Seek with a continuous causal K-filter by replaying the prefix."""
        return self.rebuild_to_block(index)

    def restart(self) -> AssetReport:
        with self._lock:
            self._reset_processing()
            self._raw_blocks.clear()
            self._published = self._build_current_report()
            return self._published

    def release_block_handles(self) -> None:
        with self._lock:
            self._current_samples = None
            self.asset.close()

    def update_params(self, new_config: MeterConfig) -> UpdateResult:
        """Incrementally apply a parameter change and atomically republish."""
        new_config.validate()
        with self._lock:
            old_config = self.config
            old_weights = self._weights
            old_parameter_version = self._parameter_version
            old_published = self._published
            old_block_samples = old_config.block_samples
            old_measurement = self._measurement_signature
            old_derived = self._derived_signature()
            old_raw_blocks = dict(self._raw_blocks)
            old_available = 0
            while old_available in old_raw_blocks:
                old_available += 1
            old_report_complete = self._published.complete
            old_end_sample = (
                old_raw_blocks[old_available - 1].window_end
                if old_available
                else 0
            )
            old_config_dict = old_config.to_dict()
            new_config_dict = new_config.to_dict()
            if old_config_dict == new_config_dict:
                return UpdateResult(
                    report=self._published,
                    affected_raw_blocks=(),
                    affected_derived_blocks=(),
                    reused_raw_blocks=tuple(sorted(old_raw_blocks)),
                    reslice=None,
                    parameter_version=self._parameter_version,
                )
            try:
                self.config = new_config
                self._weights = self._resolve_weights()
                new_measurement = self._signature()
                new_derived = self._derived_signature()
                diff: ResliceDiff | None = None
                if old_block_samples != new_config.block_samples:
                    diff = reslice_diff(
                        self.info.num_frames,
                        old_block_samples,
                        new_config.block_samples,
                    )
                target_recompute_count = 0
                if new_measurement != old_measurement:
                    if old_report_complete:
                        target_recompute_count = self.total_blocks
                    elif old_end_sample > 0:
                        target_recompute_count = min(
                            self.total_blocks,
                            math.ceil(old_end_sample / new_config.block_samples),
                        )
                    raw_affected = tuple(range(target_recompute_count))
                    self._raw_blocks.clear()
                    self._reset_processing()
                    if target_recompute_count:
                        self._ensure_through(target_recompute_count - 1)
                else:
                    # Retain raw values and the causal filter state.
                    self._raw_blocks = old_raw_blocks
                    raw_affected = ()
                derived_affected = (
                    tuple(range(len(self._raw_blocks)))
                    if new_derived != old_derived
                    else ()
                )
                if new_measurement != old_measurement or new_derived != old_derived:
                    self._parameter_version += 1
                report = self._build_current_report()
                self._published = report
            except BaseException:
                self.config = old_config
                self._weights = old_weights
                self._parameter_version = old_parameter_version
                self._raw_blocks = old_raw_blocks
                self._published = old_published
                self._reset_processing()
                if old_available:
                    # Restore the causal filter state without changing the
                    # already published report.
                    try:
                        self._ensure_through(old_available - 1)
                    except BaseException:
                        self._reset_processing()
                raise
            reused = tuple(
                index for index in sorted(self._raw_blocks) if index not in raw_affected
            )
            return UpdateResult(
                report=report,
                affected_raw_blocks=raw_affected,
                affected_derived_blocks=derived_affected,
                reused_raw_blocks=reused,
                reslice=diff,
                parameter_version=self._parameter_version,
            )

    def changes_to(self, new_config: MeterConfig) -> tuple[tuple[int, ...], ResliceDiff | None]:
        """Report the raw blocks that would need recomputation."""
        old_weights = self._weights
        weights = (
            tuple(float(x) for x in new_config.channel_weights)
            if new_config.channel_weights is not None
            else ChannelLayout(
                self.info.layout,
                self.info.channel_mask,
                self.info.labels,
            ).weights
        )
        old_signature = self._measurement_signature
        new_signature = (
            self.FILTER_VERSION,
            new_config.sample_rate,
            new_config.block_samples,
            new_config.short_term_samples,
            new_config.oversample_factor,
            new_config.true_peak_include_lfe,
            weights,
        )
        if old_signature == new_signature:
            return (), None
        diff = (
            reslice_diff(
                self.info.num_frames,
                self.config.block_samples,
                new_config.block_samples,
            )
            if new_config.block_samples != self.config.block_samples
            else None
        )
        return tuple(range(self.total_blocks)), diff


def compare_reports(
    online: AssetReport,
    offline: AssetReport,
    config: MeterConfig,
) -> dict[str, Any]:
    """Compare every synchronised block, failing at the first mismatch."""
    online_by_marker = {block.sync_marker: block for block in online.blocks}
    compared = 0
    for expected in offline.blocks:
        actual = online_by_marker.get(expected.sync_marker)
        if actual is None:
            raise error(
                ErrorCode.E_LUFS_MISMATCH,
                "online report is missing a synchronised observation block",
                {
                    "asset_id": offline.asset_id,
                    "sync_marker": expected.sync_marker,
                    "block_index": expected.block_index,
                    "field": "block",
                },
            )
        comparisons = (
            ("short_term_lufs", config.lufs_tolerance),
            ("integrated_lufs", config.lufs_tolerance),
            ("true_peak_dbtp", config.true_peak_tolerance),
            ("sample_peak_dbtp", config.true_peak_tolerance),
            ("gain_to_target_db", config.lufs_tolerance),
            ("block_energy", max(config.lufs_tolerance, 1e-12)),
            ("short_term_energy", max(config.lufs_tolerance, 1e-12)),
        )
        for field, tolerance in comparisons:
            left = float(getattr(actual, field))
            right = float(getattr(expected, field))
            if abs(left - right) > tolerance + 1e-15:
                raise error(
                    ErrorCode.E_LUFS_MISMATCH,
                    "online and offline observation-block values differ",
                    {
                        "asset_id": offline.asset_id,
                        "sync_marker": expected.sync_marker,
                        "block_index": expected.block_index,
                        "field": field,
                        "online": left,
                        "offline": right,
                        "tolerance": tolerance,
                    },
                )
        if actual.content_hash != expected.content_hash:
            raise error(
                ErrorCode.E_LUFS_MISMATCH,
                "online and offline content hashes differ",
                {
                    "asset_id": offline.asset_id,
                    "sync_marker": expected.sync_marker,
                    "block_index": expected.block_index,
                    "field": "content_hash",
                    "online": actual.content_hash,
                    "offline": expected.content_hash,
                },
            )
        for field in ("true_peak_ok", "gate_status"):
            if getattr(actual, field) != getattr(expected, field):
                raise error(
                    ErrorCode.E_LUFS_MISMATCH,
                    "online and offline observation-block state differs",
                    {
                        "asset_id": offline.asset_id,
                        "sync_marker": expected.sync_marker,
                        "block_index": expected.block_index,
                        "field": field,
                        "online": getattr(actual, field),
                        "offline": getattr(expected, field),
                    },
                )
        compared += 1
    if not online.complete:
        raise error(
            ErrorCode.E_LUFS_MISMATCH,
            "online report did not reach the end of the asset",
            {
                "asset_id": online.asset_id,
                "complete": online.complete,
                "block_count": online.block_count,
            },
        )
    return {
        "asset_id": offline.asset_id,
        "blocks_compared": compared,
        "lufs_tolerance": config.lufs_tolerance,
        "true_peak_tolerance": config.true_peak_tolerance,
        "first_mismatch": None,
    }


def analyze_offline(
    path: str,
    *,
    config: MeterConfig,
    asset_id: str | None = None,
) -> AssetReport:
    meter = AssetMeter(
        path,
        config=config,
        asset_id=asset_id,
        enforce_budget=False,
    )
    return meter.process_all()
