"""Playback controls with explicit handle release and disk-only recovery."""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from .errors import ErrorCode, MeterError, error
from .metering import AssetMeter
from .models import AssetReport, BlockResult
from .snapshot import RecoveryManager


class PlaybackState(str, Enum):
    STOPPED = "stopped"
    PLAYING = "playing"
    PAUSED = "paused"
    ENDED = "ended"
    FAILED = "failed"


class PlaybackController:
    def __init__(self, meter: AssetMeter):
        self.meter = meter
        self.state = PlaybackState.STOPPED
        self.cursor_block = 0
        self.position_samples = 0
        self.last_error: MeterError | None = None

    @property
    def published(self) -> AssetReport:
        return self.meter.get_published()

    def play(self) -> PlaybackState:
        if self.state == PlaybackState.ENDED:
            self.restart()
        self.state = PlaybackState.PLAYING
        return self.state

    def pause(self) -> PlaybackState:
        if self.state == PlaybackState.PLAYING:
            self.state = PlaybackState.PAUSED
        self.meter.release_block_handles()
        return self.state

    def advance(self, blocks: int = 1) -> BlockResult | None:
        """Advance the preview by one or more complete observation blocks."""
        if self.state != PlaybackState.PLAYING:
            return None
        if blocks < 0:
            raise error(ErrorCode.E_BLOCK_NOT_READY, "blocks must be non-negative")
        last: BlockResult | None = None
        try:
            for _ in range(blocks):
                if self.cursor_block >= self.meter.total_blocks:
                    self.state = PlaybackState.ENDED
                    self.meter.release_block_handles()
                    return last
                last = self.meter.process_block(self.cursor_block)
                self.position_samples = last.window_end
                self.cursor_block += 1
            if self.cursor_block >= self.meter.total_blocks:
                self.state = PlaybackState.ENDED
                self.meter.release_block_handles()
            return last
        except BaseException as exc:
            self.state = PlaybackState.FAILED
            self.last_error = (
                exc
                if isinstance(exc, MeterError)
                else error(ErrorCode.E_TASK_FAILED, str(exc))
            )
            self.meter.release_block_handles()
            raise

    def pause_and_release(self) -> PlaybackState:
        return self.pause()

    def seek_to_block(self, block_index: int) -> BlockResult:
        """Replay the causal prefix, then resume from the requested block."""
        try:
            self.meter.release_block_handles()
            block = self.meter.seek_to_block(block_index)
            self.cursor_block = block_index + 1
            self.position_samples = block.window_end
            self.state = PlaybackState.PAUSED
            self.meter.release_block_handles()
            return block
        except BaseException as exc:
            self.state = PlaybackState.FAILED
            self.last_error = (
                exc
                if isinstance(exc, MeterError)
                else error(ErrorCode.E_TASK_FAILED, str(exc))
            )
            self.meter.release_block_handles()
            raise

    def seek_to_sample(self, sample: int) -> BlockResult:
        if sample < 0:
            raise error(
                ErrorCode.E_BLOCK_NOT_READY,
                "sample position must be non-negative",
                {"sample": sample},
            )
        block_index = min(
            max(0, sample // self.meter.config.block_samples),
            max(0, self.meter.total_blocks - 1),
        )
        return self.seek_to_block(block_index)

    def restart(self) -> AssetReport:
        self.meter.release_block_handles()
        report = self.meter.restart()
        self.cursor_block = 0
        self.position_samples = 0
        self.state = PlaybackState.STOPPED
        self.last_error = None
        return report


def recover_from_snapshot(
    snapshot_dir: str | Path,
    asset_id: str,
    *,
    expected_parameter_version: int | None = None,
    expected_config_hash: str | None = None,
) -> AssetReport:
    """The only supported strong-termination recovery path."""
    return RecoveryManager(snapshot_dir).recover(
        asset_id,
        expected_parameter_version=expected_parameter_version,
        expected_config_hash=expected_config_hash,
    )
