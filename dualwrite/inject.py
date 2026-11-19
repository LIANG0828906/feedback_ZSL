from __future__ import annotations

import heapq
import math
import random
import threading
import time
import uuid
from dataclasses import dataclass, replace
from typing import Any

from .config import InjectionConfig


@dataclass(frozen=True)
class Delivery:
    delivery_id: str
    source_site: str
    target_site: str
    change: dict[str, Any]
    due_at: float
    duplicate_of: str | None = None

    @property
    def change_key(self) -> str:
        return f"{self.source_site}:{int(self.change['change_id'])}"

    @property
    def source_change_id(self) -> int:
        return int(self.change["change_id"])


class DeliveryScheduler:
    """Delays, reorders and duplicates changes without changing their identity."""

    def __init__(self, config: InjectionConfig):
        self.config = config
        self._rng = random.Random(config.seed)
        self._heap: list[tuple[float, int, Delivery]] = []
        self._sequence = 0
        self._lock = threading.Lock()
        self._started = time.monotonic()

    def _interruption_end(self, timestamp: float) -> float:
        interruption = self.config.interruption
        if interruption.start_after_seconds is None or interruption.duration_seconds <= 0:
            return timestamp
        start = self._started + interruption.start_after_seconds
        duration = interruption.duration_seconds
        if timestamp < start:
            return timestamp
        repeat = interruption.repeat_every_seconds
        if repeat > 0:
            period = duration + repeat
            cycles = max(0, math.floor((timestamp - start) / period))
            cycle_start = start + cycles * period
            cycle_end = cycle_start + duration
            if cycle_start <= timestamp < cycle_end:
                return cycle_end
            return timestamp
        end = start + duration
        return end if timestamp < end else timestamp

    def _due_time(self) -> float:
        now = time.monotonic()
        if not self.config.enabled:
            return now
        delay_ms = self._rng.uniform(
            self.config.delay_min_ms, max(self.config.delay_min_ms, self.config.delay_max_ms)
        )
        due = now + delay_ms / 1000.0
        if self.config.reorder_window_ms > 0:
            due += self._rng.uniform(0, self.config.reorder_window_ms / 1000.0)
        return self._interruption_end(due)

    def enqueue(
        self, source_site: str, target_site: str, change: dict[str, Any]
    ) -> list[Delivery]:
        with self._lock:
            original_id = uuid.uuid4().hex
            due = self._due_time()
            original = Delivery(
                delivery_id=original_id,
                source_site=source_site,
                target_site=target_site,
                change=change,
                due_at=due,
            )
            deliveries = [original]
            if self.config.enabled and self._rng.random() < self.config.duplicate_rate:
                duplicate_due = self._due_time()
                deliveries.append(
                    Delivery(
                        delivery_id=uuid.uuid4().hex,
                        source_site=source_site,
                        target_site=target_site,
                        change=change,
                        due_at=duplicate_due,
                        duplicate_of=original_id,
                    )
                )
            for delivery in deliveries:
                self._sequence += 1
                heapq.heappush(
                    self._heap, (delivery.due_at, self._sequence, delivery)
                )
            return deliveries

    def pop_ready(self, now: float | None = None) -> Delivery | None:
        current = time.monotonic() if now is None else now
        with self._lock:
            if not self._heap or self._heap[0][0] > current:
                return None
            interruption_end = self._interruption_end(current)
            if interruption_end > current:
                due_at, sequence, delivery = heapq.heappop(self._heap)
                heapq.heappush(
                    self._heap,
                    (
                        max(due_at, interruption_end),
                        sequence,
                        replace(delivery, due_at=max(due_at, interruption_end)),
                    ),
                )
                return None
            _, _, delivery = heapq.heappop(self._heap)
            return delivery

    def requeue(self, delivery: Delivery, delay_ms: int = 100) -> None:
        with self._lock:
            self._sequence += 1
            requeued = replace(delivery, due_at=time.monotonic() + delay_ms / 1000.0)
            heapq.heappush(
                self._heap, (requeued.due_at, self._sequence, requeued)
            )

    def next_due_in(self) -> float | None:
        with self._lock:
            if not self._heap:
                return None
            return max(0.0, self._heap[0][0] - time.monotonic())

    @property
    def pending(self) -> int:
        with self._lock:
            return len(self._heap)
