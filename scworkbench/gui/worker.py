from __future__ import annotations

import json
import threading
import traceback
from pathlib import Path
from typing import Any

from PySide6.QtCore import QThread, Signal

from ..engine import EngineConfig, SessionEngine
from ..loopback import LoopbackUdpClient
from ..models import SampleSession
from ..replaylog import ReplayLogWriter
from ..runner import execute_paths


class PlaybackWorker(QThread):
    snapshot_ready = Signal(object)
    frame_ready = Signal(object, object)
    completed = Signal(object)
    failed = Signal(str)

    def __init__(
        self,
        engine: SessionEngine,
        log_writer: ReplayLogWriter | None = None,
        interval_ms: int = 55,
        parent: Any = None,
    ) -> None:
        super().__init__(parent)
        self.engine = engine
        self.log_writer = log_writer
        self.interval_ms = interval_ms
        self._condition = threading.Condition()
        self._playback_paused = False
        self._stop_requested = False
        self._log_cursor = 0
        self._last_frame_seq = -1

    def set_playback_paused(self, paused: bool) -> None:
        with self._condition:
            self._playback_paused = paused
            self._condition.notify_all()

    def request_stop(self) -> None:
        with self._condition:
            self._stop_requested = True
            self._condition.notify_all()

    def run(self) -> None:
        finished_normally = False
        try:
            while True:
                with self._condition:
                    while self._playback_paused and not self._stop_requested:
                        self._condition.wait(timeout=0.2)
                    if self._stop_requested:
                        break
                if self.engine.finished:
                    finished_normally = True
                    break
                self.engine.step()
                snapshot = self.engine.ui_snapshot()
                self._flush_log()
                self.snapshot_ready.emit(snapshot)
                if snapshot["frames"]:
                    last = snapshot["frames"][-1]
                    if last["frame_seq"] != self._last_frame_seq:
                        self._last_frame_seq = last["frame_seq"]
                        self.frame_ready.emit(self.engine.current_image, last)
                self.msleep(self.interval_ms)
        except Exception:
            if self.log_writer is not None:
                self.log_writer.abort()
            self.failed.emit(traceback.format_exc())
            return
        if finished_normally:
            try:
                self._flush_log()
                result = self.engine.to_run_result("realtime")
                if self.log_writer is not None:
                    self.log_writer.append_result(result)
                self.snapshot_ready.emit(self.engine.ui_snapshot())
                self.completed.emit(result)
            except Exception:
                self.failed.emit(traceback.format_exc())
        elif self.log_writer is not None:
            self.log_writer.abort()

    def _flush_log(self) -> None:
        if self.log_writer is None:
            return
        events = self.engine.events_since(self._log_cursor)
        for event in events:
            self.log_writer.append(event)
        self._log_cursor += len(events)


class PathComparisonWorker(QThread):
    completed = Signal(object, object)
    failed = Signal(str)

    def __init__(
        self,
        sample: SampleSession,
        config: EngineConfig,
        checkpoint_directory: str | Path,
        output_path: str | Path,
        relay_port: int | None = None,
        parent: Any = None,
    ) -> None:
        super().__init__(parent)
        self.sample = sample
        self.config = config
        self.checkpoint_directory = Path(checkpoint_directory)
        self.output_path = Path(output_path)
        self.relay_port = relay_port

    def run(self) -> None:
        try:
            execution = execute_paths(
                self.sample,
                self.config,
                self.checkpoint_directory,
                recovery_after_frames=18,
                transport_factory=(
                    lambda: LoopbackUdpClient(port=self.relay_port)
                    if self.relay_port is not None
                    else None
                ),
            )
            document = execution.to_dict()
            self.output_path.parent.mkdir(parents=True, exist_ok=True)
            self.output_path.write_text(
                json.dumps(
                    document,
                    ensure_ascii=False,
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
            self.completed.emit(execution.comparison, document)
        except Exception:
            self.failed.emit(traceback.format_exc())
