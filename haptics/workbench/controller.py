from __future__ import annotations

import threading
from dataclasses import replace as dataclass_replace
from pathlib import Path
from typing import Any, Callable

from ..incremental import (
    IncrementalResult,
    edit_channel_capability,
    edit_event_priority,
    persisted_packet_index,
)
from ..models import DeviceCapabilities, EventStream
from ..pipeline import (
    RunResult,
    VerificationSummary,
    retry_alignment_packet,
    run_realtime,
    run_resume,
    verify_three_paths,
)
from ..sample import sample_capabilities, sample_events
from ..transport import FaultPlan, MockDeviceService
from ..persistence import CheckpointStore


class OperationCancelled(RuntimeError):
    pass


class StopPauseGate:
    """Cooperative stop/pause gate checked between durable windows."""

    def __init__(self) -> None:
        self._condition = threading.Condition()
        self._paused = False
        self._stopped = False

    def __call__(self, packet_seq: int) -> None:
        with self._condition:
            if self._stopped:
                raise OperationCancelled(f"stopped before packet {packet_seq}")
            while self._paused and not self._stopped:
                self._condition.wait(timeout=0.1)
            if self._stopped:
                raise OperationCancelled(f"stopped before packet {packet_seq}")

    @property
    def paused(self) -> bool:
        with self._condition:
            return self._paused

    def pause(self) -> None:
        with self._condition:
            self._paused = True

    def resume(self) -> None:
        with self._condition:
            self._paused = False
            self._condition.notify_all()

    def stop(self) -> None:
        with self._condition:
            self._stopped = True
            self._paused = False
            self._condition.notify_all()


class WorkbenchController:
    def __init__(self, workdir: str | Path, port: int = 0):
        self.workdir = Path(workdir)
        self.workdir.mkdir(parents=True, exist_ok=True)
        self.stream: EventStream = sample_events()
        self.capabilities: DeviceCapabilities = sample_capabilities()
        self.store = CheckpointStore(self.workdir)
        self.fault_plan = FaultPlan.sample()
        try:
            self.service = MockDeviceService(port=port, fault_plan=self.fault_plan).start()
        except Exception:
            self.store.close()
            raise
        self.result: RunResult | None = None
        self.incremental: IncrementalResult | None = None
        self.verification: VerificationSummary | None = None
        self._sequence = 0
        self._lock = threading.RLock()
        self.last_error: str | None = None

    @property
    def port(self) -> int:
        return self.service.port

    def _run_id(self, mode: str) -> str:
        with self._lock:
            self._sequence += 1
            return f"ui-{mode}-{self._sequence:04d}"

    def current_packets(self):
        if self.incremental is not None:
            return self.incremental.packets
        if self.result is not None:
            return self.result.packets
        return ()

    def current_decisions(self):
        if self.incremental is not None:
            return self.incremental.decisions
        if self.result is not None:
            return self.result.decisions
        return ()

    def run_realtime(self, gate: Callable[[int], None] | None = None) -> RunResult:
        result = run_realtime(
            self.stream,
            self.capabilities,
            store=self.store,
            service=self.service,
            timeout_ms=30.0,
            parallel=True,
            run_id=self._run_id("realtime"),
            pause_gate=gate,
            reset_store=True,
        )
        with self._lock:
            self.result = result
            self.incremental = None
        return result

    def run_resume(self, gate: Callable[[int], None] | None = None) -> RunResult:
        result = run_resume(
            self.stream,
            self.capabilities,
            self.store,
            service=self.service,
            timeout_ms=30.0,
            parallel=True,
            run_id=self._run_id("resume"),
            pause_gate=gate,
        )
        with self._lock:
            self.result = result
            self.incremental = None
        return result

    def verify(self) -> VerificationSummary:
        summary = verify_three_paths(
            self.stream,
            self.capabilities,
            self.workdir / "verification",
            fault_plan=self.fault_plan,
            timeout_ms=30.0,
            parallel=True,
        )
        with self._lock:
            self.verification = summary
            self.result = summary.realtime
            self.incremental = None
        return summary

    def _persisted(self):
        loaded = self.store.load_valid(
            expected_capability_version=self.capabilities.capability_version,
            expected_kernel_version="1.0.0",
            invalidate_on_error=False,
        )
        if not loaded.valid:
            return {}
        return persisted_packet_index(loaded.checkpoints)

    def _dependencies(self):
        if self.incremental is not None:
            return self.incremental.dependencies
        if self.result is not None and self.result.dependencies is not None:
            return self.result.dependencies
        return None

    def edit_priority(self, event_id: str, priority: int) -> IncrementalResult:
        graph = self._dependencies()
        if graph is None:
            raise RuntimeError("run the kernel before editing; dependency graph is unavailable")
        updated = edit_event_priority(
            self.stream,
            self.capabilities,
            event_id,
            priority,
            old_graph=graph,
            persisted=self._persisted(),
            parallel=True,
        )
        updated = self._preserve_states(updated)
        self.stream = updated.stream
        self.capabilities = updated.capabilities
        self.incremental = updated
        return updated

    def edit_capability(
        self,
        channel: int,
        *,
        min_event_interval_ms: float | None = None,
        max_events_per_packet: int | None = None,
        packet_window_ms: tuple[float, float] | None = None,
    ) -> IncrementalResult:
        graph = self._dependencies()
        if graph is None:
            raise RuntimeError("run the kernel before editing; dependency graph is unavailable")
        updated = edit_channel_capability(
            self.stream,
            self.capabilities,
            channel,
            min_event_interval_ms=min_event_interval_ms,
            max_events_per_packet=max_events_per_packet,
            packet_window_ms=packet_window_ms,
            old_graph=graph,
            persisted=self._persisted(),
            parallel=True,
        )
        updated = self._preserve_states(updated)
        self.stream = updated.stream
        self.capabilities = updated.capabilities
        self.incremental = updated
        return updated

    def _preserve_states(self, updated: IncrementalResult) -> IncrementalResult:
        previous = {}
        if self.result is not None:
            previous = {
                (packet.channel, packet.local_index): packet
                for packet in self.result.packets
            }
        packets = tuple(
            dataclass_replace(
                packet,
                state=previous[packet.cache_key].state
                if packet.cache_key in previous
                else packet.state,
            )
            for packet in updated.packets
        )
        return dataclass_replace(updated, packets=packets)

    def retry(self, packet_seq: int) -> RunResult:
        if self.result is None:
            raise RuntimeError("no execution result is available")
        updated = retry_alignment_packet(
            self.result,
            packet_seq,
            self.service,
            timeout_ms=30.0,
            run_id=self._run_id("retry"),
        )
        self.result = updated
        return updated

    def close(self) -> None:
        try:
            self.service.stop()
        finally:
            self.store.close()

