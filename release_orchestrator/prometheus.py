"""Prometheus text exposition backed by persisted release state."""

from __future__ import annotations

from prometheus_client import CollectorRegistry, generate_latest
from prometheus_client.core import GaugeMetricFamily

from .service import Orchestrator


class ReleaseCollector:
    """Custom collector so every scrape reads the source-of-truth SQLite state."""

    def __init__(self, orchestrator: Orchestrator) -> None:
        self.orchestrator = orchestrator

    def collect(self):
        current_batch = GaugeMetricFamily(
            "release_orchestrator_current_batch",
            "Current or last decided gray batch for a release",
            labels=["release_id"],
        )
        released_instances = GaugeMetricFamily(
            "release_orchestrator_released_instances",
            "Number of instances in batches that have been released",
            labels=["release_id"],
        )
        threshold_instances = GaugeMetricFamily(
            "release_orchestrator_threshold_instances",
            "Number of distinct instances with an instance-level violation",
            labels=["release_id"],
        )
        abort_count = GaugeMetricFamily(
            "release_orchestrator_abort_count",
            "Number of aborted batch decisions",
            labels=["release_id"],
        )
        rollback_in_progress = GaugeMetricFamily(
            "release_orchestrator_rollback_in_progress",
            "1 when rollback is required and not yet verified complete",
            labels=["release_id"],
        )
        version_distribution = GaugeMetricFamily(
            "release_orchestrator_parameter_version",
            "Latest reported parameter-version distribution by instance count",
            labels=["release_id", "version"],
        )

        for status in self.orchestrator.list_releases():
            release_id = status["release_id"]
            current_batch.add_metric([release_id], status["current_batch"])
            released_instances.add_metric(
                [release_id], status["released_instance_count"]
            )
            threshold_instances.add_metric(
                [release_id], status["threshold_instance_count"]
            )
            abort_count.add_metric([release_id], status["abort_count"])
            rollback_in_progress.add_metric(
                [release_id], 1 if status["rollback"]["in_progress"] else 0
            )
            for version, count in status["version_distribution"].items():
                version_distribution.add_metric([release_id, version], count)

        yield current_batch
        yield released_instances
        yield threshold_instances
        yield abort_count
        yield rollback_in_progress
        yield version_distribution


def render_metrics(orchestrator: Orchestrator) -> bytes:
    registry = CollectorRegistry()
    registry.register(ReleaseCollector(orchestrator))
    return generate_latest(registry)
