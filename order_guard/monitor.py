from __future__ import annotations

import logging
import threading

from .config import Config
from .control import ControlPlane
from .metrics import Metrics
from .runtime import RuntimeStats
from .sli import SLIRegistry

LOGGER = logging.getLogger("order_guard.monitor")


class StopToken:
    def __init__(self) -> None:
        self.event = threading.Event()

    def stop(self) -> None:
        self.event.set()


class SLIMonitor(threading.Thread):
    def __init__(
        self,
        config: Config,
        registry: SLIRegistry,
        control: ControlPlane,
        metrics: Metrics,
        stop: StopToken,
    ) -> None:
        super().__init__(name="order-guard-sli-monitor", daemon=True)
        self.config = config
        self.registry = registry
        self.control = control
        self.metrics = metrics
        self.stop_token = stop
        self._active_breaches: dict[str, bool] = {
            name: False for name in config.slis
        }

    def evaluate_once(self) -> None:
        snapshots = self.registry.snapshots()
        for name, snapshot in snapshots.items():
            value = snapshot.value
            numeric_value = float(value) if value is not None else 0.0
            self.metrics.set(
                "order_guard_sli_value", numeric_value, {"sli": name}
            )
            self.metrics.set(
                "order_guard_error_budget_consumed_ratio",
                snapshot.error_budget_consumed,
                {"sli": name},
            )
            self.metrics.set(
                "order_guard_error_budget_remaining_ratio",
                snapshot.error_budget_remaining,
                {"sli": name},
            )
            previous_consumed_peak = self.metrics.get_gauge(
                "order_guard_error_budget_peak_consumed_ratio",
                {"sli": name},
            )
            if (
                previous_consumed_peak is None
                or snapshot.error_budget_consumed > previous_consumed_peak
            ):
                self.metrics.set(
                    "order_guard_error_budget_peak_consumed_ratio",
                    snapshot.error_budget_consumed,
                    {"sli": name},
                )
            self.metrics.set(
                "order_guard_sli_samples", snapshot.samples, {"sli": name}
            )
            previous_peak = self.metrics.get_gauge(
                "order_guard_sli_peak_value", {"sli": name}
            )
            if previous_peak is None or numeric_value > previous_peak:
                self.metrics.set(
                    "order_guard_sli_peak_value",
                    numeric_value,
                    {"sli": name},
                )
            was_breached = self._active_breaches[name]
            if snapshot.breached and not was_breached:
                self.metrics.inc(
                    "order_guard_slo_breaches_total", {"sli": name}
                )
            self._active_breaches[name] = snapshot.breached
            self.control.update_sli(self.config.slis[name], snapshot)

    def run(self) -> None:
        interval = self.config.sli_evaluation_interval_ms / 1000.0
        while not self.stop_token.event.wait(interval):
            try:
                self.evaluate_once()
            except Exception:
                LOGGER.exception("SLI evaluation failed")


class CapacityMonitor(threading.Thread):
    def __init__(
        self,
        config: Config,
        control: ControlPlane,
        metrics: Metrics,
        runtime: RuntimeStats,
        stop: StopToken,
    ) -> None:
        super().__init__(name="order-guard-capacity-monitor", daemon=True)
        self.config = config
        self.control = control
        self.metrics = metrics
        self.runtime = runtime
        self.stop_token = stop
        self._last_busy = metrics.get_counter(
            "order_guard_sqlite_busy_events_total"
        )
        self._enter_streak = 0
        self._recover_streak = 0
        self._last_sample: dict[str, object] = {}

    def _thresholds(self) -> dict[str, object]:
        capacity = self.config.capacity
        return {
            "thread_occupancy_threshold": capacity.thread_occupancy_threshold,
            "queue_depth_threshold": capacity.queue_depth_threshold,
            "sqlite_busy_events_threshold": capacity.sqlite_busy_events_threshold,
            "enter_after_samples": capacity.enter_after_samples,
            "recover_thread_occupancy": capacity.recover_thread_occupancy,
            "recover_queue_depth": capacity.recover_queue_depth,
            "sqlite_busy_recover_events": capacity.sqlite_busy_recover_events,
            "recover_after_samples": capacity.recover_after_samples,
        }

    def sample_once(self) -> dict[str, object]:
        active_raw = self.metrics.get_gauge("order_guard_active_queries")
        active_queries = int(active_raw or 0)
        queue_depth, occupancy = self.runtime.observe_queue(active_queries)
        busy_total = self.metrics.get_counter("order_guard_sqlite_busy_events_total")
        busy_delta = max(0, int(busy_total - self._last_busy))
        self._last_busy = busy_total

        capacity = self.config.capacity
        overloaded = (
            occupancy >= capacity.thread_occupancy_threshold
            or queue_depth >= capacity.queue_depth_threshold
            or busy_delta >= capacity.sqlite_busy_events_threshold
        )
        recovered = (
            occupancy <= capacity.recover_thread_occupancy
            and queue_depth <= capacity.recover_queue_depth
            and busy_delta <= capacity.sqlite_busy_recover_events
        )
        if self.control.state.capacity_protected:
            self._enter_streak = 0
            if recovered:
                self._recover_streak += 1
            else:
                self._recover_streak = 0
        else:
            self._recover_streak = 0
            if overloaded:
                self._enter_streak += 1
            else:
                self._enter_streak = 0

        sample = {
            "thread_occupancy": round(occupancy, 6),
            "queue_depth": queue_depth,
            "inflight_total": self.runtime.total(),
            "active_queries": active_queries,
            "sqlite_busy_events_interval": busy_delta,
            "sqlite_busy_events_total": int(busy_total),
            "overloaded": overloaded,
            "recovered": recovered,
            "enter_streak": self._enter_streak,
            "recover_streak": self._recover_streak,
        }
        self._last_sample = sample
        if (
            not self.control.state.capacity_protected
            and self._enter_streak >= capacity.enter_after_samples
        ):
            reason = (
                "thread_occupancy"
                if occupancy >= capacity.thread_occupancy_threshold
                else (
                    "queue_depth"
                    if queue_depth >= capacity.queue_depth_threshold
                    else "sqlite_busy_events"
                )
            )
            if self.control.update_capacity(
                protected=True,
                reason=reason,
                sample=sample,
                thresholds=self._thresholds(),
            ):
                LOGGER.warning("capacity protection entered: %s", sample)
                self._enter_streak = 0
        elif (
            self.control.state.capacity_protected
            and self._recover_streak >= capacity.recover_after_samples
        ):
            if self.control.update_capacity(
                protected=False,
                reason="signals_returned_to_safe_range",
                sample=sample,
                thresholds=self._thresholds(),
            ):
                LOGGER.warning("capacity protection exited: %s", sample)
                self._recover_streak = 0
        return sample

    def run(self) -> None:
        interval = self.config.capacity.sample_interval_ms / 1000.0
        while not self.stop_token.event.wait(interval):
            try:
                self.sample_once()
            except Exception:
                LOGGER.exception("capacity sample failed")
