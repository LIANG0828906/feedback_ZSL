"""High-level service facade shared by HTTP and CLI frontends."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .core import build_snapshot, calculate_window
from .data import (
    SampleSet,
    load_handover,
    load_samples,
    normalize_handover,
    parse_timestamp,
)
from .errors import HandoverError
from .metrics import render_metrics
from .policy import load_policy
from .replay import replay_snapshot
from .store import SnapshotStore


class HandoverService:
    def __init__(
        self,
        *,
        samples_path: str | Path,
        db_path: str | Path,
        handover_path: str | Path | None = None,
        policy_path: str | Path | None = None,
    ) -> None:
        self.policy = load_policy(policy_path)
        self.samples = SampleSet(load_samples(samples_path))
        self.store = SnapshotStore(db_path)
        self.handover_path = Path(handover_path) if handover_path else None

    def default_handover(self) -> dict[str, Any]:
        if self.handover_path is None:
            raise HandoverError(
                "HANDOVER_REQUIRED",
                "No handover was supplied and no default handover file was configured.",
            )
        return load_handover(self.handover_path)

    def _handover(self, raw: dict[str, Any] | None) -> dict[str, Any]:
        return normalize_handover(raw) if raw is not None else self.default_handover()

    def decide(self, raw: dict[str, Any] | None = None) -> dict[str, Any]:
        handover = self._handover(raw)
        return build_snapshot(handover, self.samples, self.policy)

    def submit(self, raw: dict[str, Any] | None = None) -> dict[str, Any]:
        snapshot = self.decide(raw)
        return self.store.insert_snapshot(snapshot)

    def get_snapshot(self, handover_id: str) -> dict[str, Any]:
        return self.store.get_snapshot(handover_id)

    def history(self, limit: int = 100) -> list[dict[str, Any]]:
        return self.store.list_snapshots(limit)

    def replay(
        self,
        handover_id: str,
        *,
        until: str | None = None,
        step_minutes: int = 1,
        include_series: bool = True,
    ) -> dict[str, Any]:
        record = self.store.get_snapshot(handover_id)
        snapshot = record["snapshot"]
        snapshot["snapshot_id"] = record["snapshot_id"]
        return replay_snapshot(
            snapshot,
            self.samples,
            until=until,
            step_minutes=step_minutes,
            include_series=include_series,
        )

    def status(self, handover_id: str | None = None) -> dict[str, Any]:
        if handover_id:
            record = self.store.get_snapshot(handover_id)
        else:
            record = self.store.get_latest_snapshot()
            if record is None:
                raise HandoverError(
                    "SNAPSHOT_NOT_FOUND",
                    "No snapshot has been committed.",
                    http_status=404,
                )
        snapshot = record["snapshot"]
        summary = snapshot["decision"]["summary"]
        try:
            replay = self.replay(
                record["handover_id"],
                include_series=False,
            )
            replay_summary = {
                "available": True,
                "first_divergence_at": replay["first_divergence_at"],
                "first_divergence_trigger_metric": replay[
                    "first_divergence_trigger_metric"
                ],
                "replay_until": replay["replay_until"],
                "actual_grade": replay["actual_grade"],
                "error_code": None,
            }
        except HandoverError as exc:
            replay_summary = {
                "available": False,
                "first_divergence_at": None,
                "first_divergence_trigger_metric": None,
                "replay_until": None,
                "actual_grade": summary["actual_grade"],
                "error_code": exc.code,
            }
        return {
            "schema_version": "1.0",
            "snapshot": {
                "snapshot_id": record["snapshot_id"],
                "handover_id": record["handover_id"],
                "handover_at": record["handover_at"],
                "created_at": record["created_at"],
            },
            "handover": {
                "handover_id": snapshot["handover_id"],
                "handover_at": snapshot["handover_at"],
                "from_operator": snapshot["from_operator"],
                "to_operator": snapshot["to_operator"],
                "policy_version": snapshot["policy_version"],
            },
            "decision_summary": summary,
            "replay": replay_summary,
            "latest_rejected_submission": self.store.latest_rejection(),
        }

    def window(
        self, service: str, start: str, end: str
    ) -> dict[str, Any]:
        start_dt = parse_timestamp(start, field="start")
        end_dt = parse_timestamp(end, field="end")
        rows = self.samples.window(service, start_dt, end_dt)
        metrics = calculate_window(rows, start_dt, end_dt)
        metrics["service"] = service
        metrics["recomputed"] = True
        return metrics

    def metrics_text(self) -> str:
        return render_metrics(self)

    def close(self) -> None:
        self.store.close()

    def health(self) -> dict[str, Any]:
        latest = self.store.get_latest_snapshot()
        return {
            "status": "ok",
            "services": list(self.samples.services),
            "sample_count": len(self.samples.samples),
            "snapshot_count": self.store.snapshot_count(),
            "latest_handover_at": latest["handover_at"] if latest else None,
        }


def resolve_data_paths(
    data_dir: str | Path,
    samples: str | Path | None,
    handover: str | Path | None,
    policy: str | Path | None,
) -> tuple[str | Path, str | Path | None, str | Path | None]:
    directory = Path(data_dir)
    sample_path: str | Path = samples or directory
    handover_path: str | Path | None = handover
    if handover_path is None:
        candidate = directory / "handover.json"
        if candidate.is_file():
            handover_path = candidate
    policy_path: str | Path | None = policy
    if policy_path is None:
        candidate = directory / "policy.json"
        if candidate.is_file():
            policy_path = candidate
    return sample_path, handover_path, policy_path


def build_service(
    *,
    data_dir: str | Path = "data",
    samples: str | Path | None = None,
    handover: str | Path | None = None,
    db_path: str | Path = "var/handover.db",
    policy: str | Path | None = None,
) -> HandoverService:
    sample_path, handover_path, policy_path = resolve_data_paths(
        data_dir, samples, handover, policy
    )
    return HandoverService(
        samples_path=sample_path,
        handover_path=handover_path,
        db_path=db_path,
        policy_path=policy_path,
    )
