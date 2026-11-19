from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import threading
import time
import traceback
from concurrent import futures
from typing import Any

import grpc
import numpy as np

from proto import training_pb2 as pb
from proto import training_pb2_grpc as pb_grpc
from train import db
from train import metrics
from train.common import (
    expected_shard_assignments,
    load_config,
    load_data_metadata,
    load_manifest_for_config,
    parameter_names,
    resolve_path,
)


LOG = logging.getLogger("coordinator")

OK = "OK"
INVALID_ARGUMENT = "INVALID_ARGUMENT"
INVALID_GRADIENT = "INVALID_GRADIENT"
UNKNOWN_WORKER = "UNKNOWN_WORKER"
UNKNOWN_SHARD = "UNKNOWN_SHARD"
WORKER_IDENTITY_CONFLICT = "WORKER_IDENTITY_CONFLICT"
SHARD_PARAMETER_SET_CONFLICT = "SHARD_PARAMETER_SET_CONFLICT"
SHARD_OWNERSHIP_CONFLICT = "SHARD_OWNERSHIP_CONFLICT"
NOT_REGISTERED = "NOT_REGISTERED"
INVALID_BASE_VERSION = "INVALID_BASE_VERSION"
STALE_VERSION = "STALE_VERSION"
DUPLICATE_GRADIENT = "DUPLICATE_GRADIENT"
OUTDATED_STEP = "OUTDATED_STEP"
PIPELINE_BACKPRESSURE = "PIPELINE_BACKPRESSURE"
TAKEOVER_IN_PROGRESS = "TAKEOVER_IN_PROGRESS"
SHARD_UNAVAILABLE = "SHARD_UNAVAILABLE"
VERSION_TIMEOUT = "VERSION_TIMEOUT"
NOT_READY = "NOT_READY"
INTERNAL = "INTERNAL"


class TrainingCoordinator:
    def __init__(self, config_path: str = "config.json"):
        self.config = load_config(config_path)
        self.manifest = load_manifest_for_config(self.config)
        self.data_metadata = load_data_metadata(self.config)
        self.db_path = db.initialize_database(self.config)
        self.connection = db.connect(self.db_path)
        self.lock = threading.RLock()
        self.condition = threading.Condition(self.lock)
        self.stop_event = threading.Event()
        self.param_names = parameter_names(self.manifest)
        self.param_index = {
            name: index for index, name in enumerate(self.param_names)
        }
        self.rows, self.classes = (
            int(self.manifest["model"]["feature_dim"]),
            int(self.manifest["model"]["num_classes"]),
        )
        self.learning_rate = float(
            self.manifest["hyperparameters"]["learning_rate"]
        )
        self.configured_steps = int(self.manifest["hyperparameters"]["steps"])
        self.worker_order = [
            str(worker["worker_id"]) for worker in self.config["workers"]
        ]
        self.expected_worker_ids = set(self.worker_order)
        self.shard_ids = [
            str(shard["shard_id"]) for shard in self.config["shards"]
        ]
        self.expected_assignments = expected_shard_assignments(
            self.manifest, self.shard_ids
        )
        runtime = self.config["runtime"]
        self.max_stale_steps = int(runtime["max_stale_steps"])
        self.max_consecutive_stale = int(runtime["max_consecutive_stale"])
        self.max_pending_steps = int(runtime["max_pending_steps"])
        self.max_pending_submissions = int(runtime["max_pending_submissions"])
        self.submit_wait_seconds = float(runtime["submit_wait_seconds"])
        self.worker_heartbeat_timeout = (
            float(runtime["worker_heartbeat_timeout_ms"]) / 1000.0
        )
        self.shard_heartbeat_timeout = (
            float(runtime["shard_heartbeat_timeout_ms"]) / 1000.0
        )
        self.monitor_interval = float(runtime["monitor_interval_ms"]) / 1000.0
        self.takeover_rpc_timeout = float(
            runtime["takeover_rpc_timeout_seconds"]
        )

        now = time.time()
        self.workers: dict[str, dict[str, Any]] = {}
        for row in self.connection.execute(
            "SELECT * FROM workers ORDER BY worker_id"
        ):
            record = dict(row)
            self.workers[record["worker_id"]] = record

        self.shards: dict[str, dict[str, Any]] = {}
        for row in self.connection.execute(
            "SELECT * FROM shards ORDER BY shard_id"
        ):
            record = dict(row)
            record["declared_parameter_names"] = sorted(
                json.loads(record["declared_names"])
            )
            self.shards[record["shard_id"]] = record

        self.ownership: dict[str, tuple[str, int]] = {
            row["parameter_name"]: (row["shard_id"], int(row["generation"]))
            for row in self.connection.execute(
                "SELECT parameter_name, shard_id, generation FROM shard_ownership"
            )
        }
        self.params = db.load_current_parameters(self.connection)
        self.version = db.last_applied_step(self.connection)
        self.last_applied_step = self.version
        self.pending: dict[int, dict[str, Any]] = {}
        self.pending_count = 0
        self.takeover_active = False
        self.last_takeover_attempt = 0.0
        self.monitor_thread: threading.Thread | None = None

        with self.lock:
            self.connection.execute(
                "UPDATE shards SET last_heartbeat = ? WHERE status = 'active'",
                (now,),
            )
            self.connection.execute(
                "UPDATE workers SET last_heartbeat = ? WHERE status = 'active'",
                (now,),
            )
            for record in self.shards.values():
                if record["status"] == "active":
                    record["last_heartbeat"] = now
            for record in self.workers.values():
                if record["status"] == "active":
                    record["last_heartbeat"] = now
            self._refresh_liveness_locked(now)
            self._refresh_metrics_locked()
            self._initialize_metrics_from_db_locked()

    def close(self) -> None:
        self.stop_event.set()
        if self.monitor_thread and self.monitor_thread.is_alive():
            self.monitor_thread.join(timeout=2.0)
        with self.lock:
            self.connection.close()

    def start(self) -> None:
        self.monitor_thread = threading.Thread(
            target=self._monitor_loop, name="liveness-monitor", daemon=True
        )
        self.monitor_thread.start()

    def _now_locked(self) -> float:
        return time.time()

    def _worker_payload(self, record: dict[str, Any]) -> dict[str, Any]:
        return {
            "consecutive_stale": int(record["consecutive_stale"]),
            "data_shard_index": int(record["data_shard_index"]),
            "last_heartbeat": float(record["last_heartbeat"]),
            "lagging": bool(record["lagging"]),
            "local_step": int(record["local_step"]),
            "registered_at": float(record["registered_at"]),
            "sample_count": int(record["sample_count"]),
            "status": record["status"],
            "total_stale": int(record["total_stale"]),
            "trigger_version_gap": int(record["trigger_version_gap"]),
            "worker_id": record["worker_id"],
        }

    def _shard_payload(self, record: dict[str, Any]) -> dict[str, Any]:
        return {
            "declared_parameter_names": record["declared_parameter_names"],
            "endpoint": record["endpoint"],
            "generation": int(record["generation"]),
            "last_heartbeat": float(record["last_heartbeat"]),
            "shard_id": record["shard_id"],
            "status": record["status"],
        }

    def _registration_reply(
        self, ok: bool, code: str, message: str, record: dict[str, Any] | None
    ) -> pb.RegistrationReply:
        return pb.RegistrationReply(
            ok=ok,
            code=code,
            message=message,
            record_json=db.json_dumps_stable(record) if record is not None else "",
        )

    def register_worker(
        self, request: pb.RegisterWorkerRequest
    ) -> pb.RegistrationReply:
        worker_id = request.worker_id.strip()
        if not worker_id or worker_id not in self.expected_worker_ids:
            return self._registration_reply(
                False,
                UNKNOWN_WORKER,
                "worker_id is not declared in config.json",
                None,
            )
        shard_index = int(request.data_shard_index)
        sample_count = int(request.sample_count)
        if shard_index < 0 or shard_index >= len(self.data_metadata["shards"]):
            return self._registration_reply(
                False,
                INVALID_ARGUMENT,
                "data_shard_index is outside the generated data set",
                None,
            )
        expected_samples = int(
            self.data_metadata["shards"][shard_index]["sample_count"]
        )
        if sample_count != expected_samples:
            return self._registration_reply(
                False,
                INVALID_ARGUMENT,
                "sample_count does not match the generated data shard",
                None,
            )
        now = time.time()
        with self.lock:
            existing = self.workers.get(worker_id)
            if existing is not None:
                if (
                    int(existing["data_shard_index"]) != shard_index
                    or int(existing["sample_count"]) != sample_count
                ):
                    return self._registration_reply(
                        False,
                        WORKER_IDENTITY_CONFLICT,
                        "worker identity already registered with another data declaration",
                        self._worker_payload(existing),
                    )
                self.connection.execute(
                    """
                    UPDATE workers
                    SET status = 'active', last_heartbeat = ?
                    WHERE worker_id = ?
                    """,
                    (now, worker_id),
                )
                existing["status"] = "active"
                existing["last_heartbeat"] = now
                return self._registration_reply(
                    True,
                    OK,
                    "existing worker registration returned",
                    self._worker_payload(existing),
                )
            self.connection.execute(
                """
                INSERT INTO workers(
                    worker_id, data_shard_index, sample_count, status,
                    registered_at, last_heartbeat
                ) VALUES (?, ?, ?, 'active', ?, ?)
                """,
                (worker_id, shard_index, sample_count, now, now),
            )
            row = self.connection.execute(
                "SELECT * FROM workers WHERE worker_id = ?", (worker_id,)
            ).fetchone()
            record = dict(row)
            self.workers[worker_id] = record
            self._refresh_metrics_locked()
            return self._registration_reply(
                True,
                OK,
                "worker registered",
                self._worker_payload(record),
            )

    def register_shard(
        self, request: pb.RegisterShardRequest
    ) -> pb.RegistrationReply:
        shard_id = request.shard_id.strip()
        if shard_id not in self.shard_ids:
            return self._registration_reply(
                False, UNKNOWN_SHARD, "shard_id is not declared in config", None
            )
        declared = sorted(set(str(name) for name in request.parameter_names))
        expected = sorted(self.expected_assignments[shard_id])
        if declared != expected:
            missing = sorted(set(expected) - set(declared))[:5]
            extra = sorted(set(declared) - set(expected))[:5]
            existing_record = self.shards.get(shard_id)
            return self._registration_reply(
                False,
                SHARD_PARAMETER_SET_CONFLICT,
                f"declared parameter set does not match name-hash assignment; "
                f"missing={missing}, extra={extra}",
                self._shard_payload(existing_record)
                if existing_record is not None
                else None,
            )
        endpoint = request.endpoint.strip()
        if not endpoint:
            return self._registration_reply(
                False, INVALID_ARGUMENT, "endpoint is required", None
            )
        now = time.time()
        with self.lock:
            existing = self.shards.get(shard_id)
            if existing is not None:
                if sorted(existing["declared_parameter_names"]) != declared:
                    return self._registration_reply(
                        False,
                        SHARD_PARAMETER_SET_CONFLICT,
                        "shard identity already registered with another parameter set",
                        self._shard_payload(existing),
                    )
                self.connection.execute(
                    """
                    UPDATE shards
                    SET status = 'active', last_heartbeat = ?, last_error = NULL
                    WHERE shard_id = ?
                    """,
                    (now, shard_id),
                )
                existing["status"] = "active"
                existing["last_heartbeat"] = now
                existing["last_error"] = None
                self._refresh_metrics_locked()
                return self._registration_reply(
                    True,
                    OK,
                    "existing shard registration returned",
                    self._shard_payload(existing),
                )

            claimed = {
                row["parameter_name"]: row["shard_id"]
                for row in self.connection.execute(
                    "SELECT parameter_name, shard_id FROM shard_ownership"
                )
            }
            overlapping = [
                name for name in declared if name in claimed and claimed[name] != shard_id
            ]
            if overlapping:
                return self._registration_reply(
                    False,
                    SHARD_OWNERSHIP_CONFLICT,
                    f"parameter names already owned by another shard: {overlapping[:5]}",
                    None,
                )
            generation = 1
            with db.transaction(self.connection):
                self.connection.execute(
                    """
                    INSERT INTO shards(
                        shard_id, endpoint, declared_names, status, generation,
                        registered_at, last_heartbeat
                    ) VALUES (?, ?, ?, 'active', ?, ?, ?)
                    """,
                    (
                        shard_id,
                        endpoint,
                        db.json_dumps_stable(declared),
                        generation,
                        now,
                        now,
                    ),
                )
                self.connection.executemany(
                    """
                    INSERT INTO shard_ownership(parameter_name, shard_id, generation)
                    VALUES (?, ?, ?)
                    ON CONFLICT(parameter_name) DO UPDATE SET
                        shard_id = excluded.shard_id,
                        generation = excluded.generation
                    """,
                    [(name, shard_id, generation) for name in declared],
                )
            row = self.connection.execute(
                "SELECT * FROM shards WHERE shard_id = ?", (shard_id,)
            ).fetchone()
            record = dict(row)
            record["declared_parameter_names"] = declared
            self.shards[shard_id] = record
            for name in declared:
                self.ownership[name] = (shard_id, generation)
            self._refresh_metrics_locked()
            return self._registration_reply(
                True, OK, "shard registered", self._shard_payload(record)
            )

    def heartbeat(self, request: pb.HeartbeatRequest) -> pb.HeartbeatReply:
        identity_type = request.identity_type.strip()
        identity_id = request.identity_id.strip()
        now = time.time()
        with self.lock:
            if identity_type == "worker":
                record = self.workers.get(identity_id)
                if record is None:
                    return pb.HeartbeatReply(
                        ok=False,
                        code=NOT_REGISTERED,
                        version=self.version,
                        takeover_in_progress=self.takeover_active,
                        message="worker is not registered",
                    )
                self.connection.execute(
                    """
                    UPDATE workers
                    SET last_heartbeat = ?, status = 'active', local_step = ?
                    WHERE worker_id = ?
                    """,
                    (now, max(0, int(request.local_step)), identity_id),
                )
                record["last_heartbeat"] = now
                record["status"] = "active"
                record["local_step"] = max(0, int(request.local_step))
            elif identity_type == "shard":
                record = self.shards.get(identity_id)
                if record is None:
                    return pb.HeartbeatReply(
                        ok=False,
                        code=NOT_REGISTERED,
                        version=self.version,
                        takeover_in_progress=self.takeover_active,
                        message="shard is not registered",
                    )
                self.connection.execute(
                    """
                    UPDATE shards
                    SET last_heartbeat = ?, status = 'active', last_error = NULL
                    WHERE shard_id = ?
                    """,
                    (now, identity_id),
                )
                record["last_heartbeat"] = now
                record["status"] = "active"
            else:
                return pb.HeartbeatReply(
                    ok=False,
                    code=INVALID_ARGUMENT,
                    version=self.version,
                    takeover_in_progress=self.takeover_active,
                    message="identity_type must be worker or shard",
                )
            self._refresh_metrics_locked()
            return pb.HeartbeatReply(
                ok=True,
                code=OK,
                version=self.version,
                takeover_in_progress=self.takeover_active,
            )

    def _note_stale_locked(
        self,
        worker_id: str,
        step: int,
        base_version: int,
        current_version: int,
        now: float,
        sample_count: int,
        gradient: bytes,
    ) -> int:
        gap = current_version - base_version
        record = self.workers[worker_id]
        previous_consecutive = int(record["consecutive_stale"])
        consecutive = previous_consecutive + 1
        lagging = int(record["lagging"])
        trigger_gap = int(record["trigger_version_gap"])
        crossed = consecutive > self.max_consecutive_stale
        if crossed:
            lagging = 1
            trigger_gap = max(trigger_gap, gap)
        with db.transaction(self.connection):
            self.connection.execute(
                """
                INSERT INTO stale_rejections(
                    worker_id, step, base_version, current_version,
                    version_gap, sample_count, gradient, code, rejected_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    worker_id,
                    step,
                    base_version,
                    current_version,
                    gap,
                    sample_count,
                    gradient,
                    STALE_VERSION,
                    now,
                ),
            )
            self.connection.execute(
                """
                UPDATE workers
                SET total_stale = total_stale + 1,
                    consecutive_stale = ?,
                    lagging = ?,
                    trigger_version_gap = ?,
                    last_lag_at = ?
                WHERE worker_id = ?
                """,
                (consecutive, lagging, trigger_gap, now, worker_id),
            )
            if crossed and previous_consecutive <= self.max_consecutive_stale:
                self.connection.execute(
                    """
                    INSERT INTO lag_events(
                        worker_id, version_gap, consecutive_stale, triggered_at
                    ) VALUES (?, ?, ?, ?)
                    """,
                    (worker_id, gap, consecutive, now),
                )
        record["total_stale"] = int(record["total_stale"]) + 1
        record["consecutive_stale"] = consecutive
        record["lagging"] = lagging
        record["trigger_version_gap"] = trigger_gap
        record["last_lag_at"] = now
        metrics.GRADIENT_SUBMISSIONS.labels(result="stale").inc()
        metrics.GRADIENT_DISCARDED.labels(reason="stale_version").inc()
        for shard_id in sorted(
            {shard_id for (shard_id, _generation) in self.ownership.values()}
        ):
            metrics.STALE_REJECTIONS.labels(shard_id=shard_id).inc()
            metrics.SHARD_GRADIENT_DISCARDED.labels(
                shard_id=shard_id, reason="stale_version"
            ).inc()
        return consecutive

    def _accepted_row(
        self, worker_id: str, step: int
    ) -> dict[str, Any] | None:
        row = self.connection.execute(
            """
            SELECT * FROM submissions
            WHERE worker_id = ? AND step = ? AND status = 'applied'
            """,
            (worker_id, step),
        ).fetchone()
        return db.row_to_dict(row)

    def _record_duplicate_locked(
        self,
        worker_id: str,
        step: int,
        base_version: int,
        current_version: int,
        returned_version: int,
        original_attempt_id: int | None,
        sample_count: int,
        gradient: bytes,
        now: float,
    ) -> None:
        self.connection.execute(
            """
            INSERT INTO duplicate_submissions(
                worker_id, step, base_version, current_version,
                original_attempt_id, returned_version, sample_count, gradient,
                received_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                worker_id,
                step,
                base_version,
                current_version,
                original_attempt_id,
                returned_version,
                sample_count,
                gradient,
                now,
            ),
        )
        metrics.GRADIENT_SUBMISSIONS.labels(result="duplicate").inc()
        metrics.GRADIENT_DISCARDED.labels(reason="duplicate").inc()
        for shard_id in sorted(
            {shard_id for (shard_id, _generation) in self.ownership.values()}
        ):
            metrics.SHARD_GRADIENT_DISCARDED.labels(
                shard_id=shard_id, reason="duplicate"
            ).inc()

    def submit_gradient(
        self, request: pb.GradientSubmission
    ) -> pb.GradientReply:
        started = time.perf_counter()
        worker_id = request.worker_id.strip()
        step = int(request.step)
        base_version = int(request.base_version)
        sample_count = int(request.sample_count)
        gradient = np.asarray(request.gradient, dtype=np.float64)
        now = time.time()
        with self.lock:
            if worker_id not in self.workers:
                return pb.GradientReply(
                    ok=False,
                    code=NOT_REGISTERED,
                    message="worker is not registered",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )
            if step < 1 or step > self.configured_steps:
                return pb.GradientReply(
                    ok=False,
                    code=INVALID_ARGUMENT,
                    message="step is outside the declared training range",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )
            if gradient.size != len(self.param_names):
                return pb.GradientReply(
                    ok=False,
                    code=INVALID_GRADIENT,
                    message=(
                        f"gradient length {gradient.size} does not match "
                        f"{len(self.param_names)} parameters"
                    ),
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )
            if not np.all(np.isfinite(gradient)):
                return pb.GradientReply(
                    ok=False,
                    code=INVALID_GRADIENT,
                    message="gradient contains non-finite values",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )
            if sample_count <= 0:
                return pb.GradientReply(
                    ok=False,
                    code=INVALID_ARGUMENT,
                    message="sample_count must be positive",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )
            if base_version < 0 or base_version > self.version:
                return pb.GradientReply(
                    ok=False,
                    code=INVALID_BASE_VERSION,
                    message="base_version is in the future or negative",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )

            applied = self._accepted_row(worker_id, step)
            if applied is not None:
                self._record_duplicate_locked(
                    worker_id,
                    step,
                    base_version,
                    self.version,
                    int(applied["applied_version"]),
                    int(applied["attempt_id"]),
                    sample_count,
                    gradient.tobytes(),
                    now,
                )
                return pb.GradientReply(
                    ok=True,
                    code=DUPLICATE_GRADIENT,
                    message="duplicate submission discarded",
                    status="duplicate_discarded",
                    applied_version=int(applied["applied_version"]),
                    latest_version=self.version,
                    discarded=True,
                    consecutive_stale=int(
                        self.workers[worker_id]["consecutive_stale"]
                    ),
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )

            bucket = self.pending.get(step)
            if bucket is not None and worker_id in bucket["submissions"]:
                bucket["duplicates"].append(
                    {
                        "worker_id": worker_id,
                        "step": step,
                        "base_version": base_version,
                        "current_version": self.version,
                        "sample_count": sample_count,
                        "gradient": gradient.tobytes(),
                        "received_at": now,
                    }
                )
                metrics.GRADIENT_SUBMISSIONS.labels(result="duplicate").inc()
                metrics.GRADIENT_DISCARDED.labels(reason="duplicate").inc()
                for shard_id in sorted(
                    {shard_id for (shard_id, _generation) in self.ownership.values()}
                ):
                    metrics.SHARD_GRADIENT_DISCARDED.labels(
                        shard_id=shard_id, reason="duplicate"
                    ).inc()
                deadline = time.monotonic() + self.submit_wait_seconds
                while self.last_applied_step < step:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        break
                    self.condition.wait(timeout=min(remaining, 0.2))
                    self._apply_ready_locked()
                applied_version = (
                    step if self.last_applied_step >= step else 0
                )
                return pb.GradientReply(
                    ok=True,
                    code=DUPLICATE_GRADIENT,
                    message=(
                        "duplicate submission discarded after pending step applied"
                        if applied_version
                        else "duplicate submission for a still-pending step discarded"
                    ),
                    status=(
                        "duplicate_discarded"
                        if applied_version
                        else "duplicate_pending"
                    ),
                    applied_version=applied_version,
                    latest_version=self.version,
                    discarded=True,
                    consecutive_stale=int(
                        self.workers[worker_id]["consecutive_stale"]
                    ),
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )

            if step <= self.last_applied_step:
                return pb.GradientReply(
                    ok=False,
                    code=OUTDATED_STEP,
                    message="step is already committed but this worker was not applied",
                    status="rejected",
                    applied_version=step,
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )

            if self.version - base_version > self.max_stale_steps:
                consecutive = self._note_stale_locked(
                    worker_id,
                    step,
                    base_version,
                    self.version,
                    now,
                    sample_count,
                    gradient.tobytes(),
                )
                return pb.GradientReply(
                    ok=False,
                    code=STALE_VERSION,
                    message=(
                        f"base_version {base_version} is older than current "
                        f"version {self.version} by more than "
                        f"{self.max_stale_steps} steps"
                    ),
                    status="stale_rejected",
                    applied_version=self.version,
                    latest_version=self.version,
                    discarded=True,
                    consecutive_stale=consecutive,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )

            if step > self.last_applied_step + self.max_pending_steps:
                return pb.GradientReply(
                    ok=False,
                    code=PIPELINE_BACKPRESSURE,
                    message="step is too far ahead of the applied version",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )
            if self.pending_count >= self.max_pending_submissions:
                return pb.GradientReply(
                    ok=False,
                    code=PIPELINE_BACKPRESSURE,
                    message="bounded pending submission queue is full",
                    status="rejected",
                    latest_version=self.version,
                    server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
                )

            if bucket is None:
                bucket = {"submissions": {}, "duplicates": [], "arrived_at": now}
                self.pending[step] = bucket
            bucket["submissions"][worker_id] = {
                "worker_id": worker_id,
                "step": step,
                "base_version": base_version,
                "sample_count": sample_count,
                "local_loss": float(request.local_loss),
                "gradient": gradient.astype(np.float64, copy=True).tobytes(),
                "submitted_at": now,
                "submitted_monotonic": time.monotonic(),
            }
            self.pending_count += 1
            self.connection.execute(
                """
                UPDATE workers
                SET consecutive_stale = 0, status = 'active', last_heartbeat = ?
                WHERE worker_id = ?
                """,
                (now, worker_id),
            )
            self.workers[worker_id]["consecutive_stale"] = 0
            self.workers[worker_id]["last_heartbeat"] = now
            self.workers[worker_id]["status"] = "active"
            self._apply_ready_locked()
            if self.last_applied_step >= step:
                status = "applied"
                applied_version = step
                message = "gradient applied in atomic step transaction"
            else:
                status = "queued"
                applied_version = 0
                message = "gradient queued for deterministic step aggregation"
            metrics.GRADIENT_SUBMISSIONS.labels(result=status).inc()
            metrics.PENDING_SUBMISSIONS.set(self.pending_count)
            return pb.GradientReply(
                ok=True,
                code=OK,
                message=message,
                status=status,
                applied_version=applied_version,
                latest_version=self.version,
                consecutive_stale=int(self.workers[worker_id]["consecutive_stale"]),
                server_elapsed_ms=(time.perf_counter() - started) * 1000.0,
            )

    def _owners_ready_locked(self) -> tuple[bool, str]:
        if self.takeover_active:
            return False, TAKEOVER_IN_PROGRESS
        dead: str | None = None
        for name in self.param_names:
            owner = self.ownership.get(name)
            if owner is None:
                return False, SHARD_UNAVAILABLE
            shard_id, _generation = owner
            shard = self.shards.get(shard_id)
            if shard is None or shard["status"] != "active":
                dead = shard_id
                break
        if dead is not None:
            self._schedule_takeover_locked(dead)
            return False, TAKEOVER_IN_PROGRESS
        return True, OK

    def _apply_ready_locked(self) -> None:
        while True:
            next_step = self.last_applied_step + 1
            bucket = self.pending.get(next_step)
            if bucket is None:
                break
            if set(bucket["submissions"].keys()) != self.expected_worker_ids:
                break
            ready, reason = self._owners_ready_locked()
            if not ready:
                metrics.PENDING_SUBMISSIONS.set(self.pending_count)
                return
            try:
                self._apply_step_locked(next_step, bucket)
            except Exception:
                LOG.error("failed to apply step %s\n%s", next_step, traceback.format_exc())
                metrics.PENDING_SUBMISSIONS.set(self.pending_count)
                return

    def _apply_step_locked(
        self, step: int, bucket: dict[str, Any]
    ) -> None:
        now = time.time()
        aggregate = np.zeros(len(self.param_names), dtype=np.float64)
        total_samples = 0
        weighted_loss = 0.0
        ordered_attempts = [
            bucket["submissions"][worker_id] for worker_id in self.worker_order
        ]
        for attempt in ordered_attempts:
            count = int(attempt["sample_count"])
            gradient = np.frombuffer(attempt["gradient"], dtype=np.float64)
            aggregate += gradient * float(count)
            total_samples += count
            weighted_loss += float(attempt["local_loss"]) * float(count)
        aggregate /= float(total_samples)
        step_loss = weighted_loss / float(total_samples)
        old_vector = np.asarray(
            [self.params[name] for name in self.param_names], dtype=np.float64
        )
        new_vector = old_vector - self.learning_rate * aggregate
        route_generation = 0
        routes: dict[str, str] = {}
        for name in self.param_names:
            shard_id, generation = self.ownership[name]
            routes[name] = shard_id
            route_generation = max(route_generation, generation)
        duration_ms = max(
            0.0,
            (time.monotonic() - min(a["submitted_monotonic"] for a in ordered_attempts))
            * 1000.0,
        )
        with db.transaction(self.connection):
            self.connection.execute(
                """
                INSERT INTO steps(
                    step, version, loss, worker_count, submission_count,
                    applied_at, duration_ms, route_generation, takeover_id
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
                """,
                (
                    step,
                    step,
                    step_loss,
                    len(ordered_attempts),
                    len(ordered_attempts),
                    now,
                    duration_ms,
                    route_generation,
                ),
            )
            for attempt in ordered_attempts:
                latency_ms = max(
                    0.0, (now - float(attempt["submitted_at"])) * 1000.0
                )
                self.connection.execute(
                    """
                    INSERT INTO submissions(
                        worker_id, step, base_version, sample_count, local_loss,
                        gradient, status, code, applied_version, submitted_at,
                        applied_at, latency_ms
                    ) VALUES (?, ?, ?, ?, ?, ?, 'applied', ?, ?, ?, ?, ?)
                    """,
                    (
                        attempt["worker_id"],
                        step,
                        int(attempt["base_version"]),
                        int(attempt["sample_count"]),
                        float(attempt["local_loss"]),
                        attempt["gradient"],
                        OK,
                        step,
                        float(attempt["submitted_at"]),
                        now,
                        latency_ms,
                    ),
                )
            self.connection.executemany(
                """
                INSERT INTO routes(step, parameter_name, shard_id, generation)
                VALUES (?, ?, ?, ?)
                """,
                [
                    (step, name, routes[name], route_generation)
                    for name in self.param_names
                ],
            )
            self.connection.executemany(
                "UPDATE parameters SET value = ? WHERE parameter_name = ?",
                [
                    (float(new_vector[index]), name)
                    for index, name in enumerate(self.param_names)
                ],
            )
            self.connection.executemany(
                """
                INSERT INTO parameter_history(version, parameter_name, value)
                VALUES (?, ?, ?)
                """,
                [
                    (step, name, float(new_vector[index]))
                    for index, name in enumerate(self.param_names)
                ],
            )
            for duplicate in bucket["duplicates"]:
                self.connection.execute(
                    """
                    INSERT INTO duplicate_submissions(
                        worker_id, step, base_version, current_version,
                        original_attempt_id, returned_version, sample_count,
                        gradient, received_at
                    ) VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?)
                    """,
                    (
                        duplicate["worker_id"],
                        duplicate["step"],
                        duplicate["base_version"],
                        duplicate["current_version"],
                        step,
                        duplicate["sample_count"],
                        duplicate["gradient"],
                        duplicate["received_at"],
                    ),
                )
            db.set_metadata_value(self.connection, "last_applied_step", str(step))
            db.set_metadata_value(self.connection, "last_applied_version", str(step))

        for index, name in enumerate(self.param_names):
            self.params[name] = float(new_vector[index])
        self.version = step
        self.last_applied_step = step
        self.pending.pop(step, None)
        self.pending_count -= len(ordered_attempts)
        metrics.STEP_DURATION.observe(duration_ms / 1000.0)
        metrics.PARAM_VERSION.set(step)
        metrics.PENDING_SUBMISSIONS.set(self.pending_count)
        for shard_id in sorted(set(routes.values())):
            metrics.SHARD_GRADIENT_SUBMISSIONS.labels(shard_id=shard_id).inc()
        self.condition.notify_all()
        LOG.info(
            "step=%s loss=%.8f workers=%s duration_ms=%.3f routes=%s",
            step,
            step_loss,
            len(ordered_attempts),
            duration_ms,
            {shard_id: list(routes.values()).count(shard_id) for shard_id in sorted(set(routes.values()))},
        )

    def wait_for_version(
        self, request: pb.WaitForVersionRequest
    ) -> pb.WaitForVersionReply:
        target = int(request.target_version)
        timeout_ms = max(0, int(request.timeout_ms))
        deadline = time.monotonic() + timeout_ms / 1000.0
        with self.condition:
            self._apply_ready_locked()
            while self.version < target:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                self.condition.wait(timeout=min(remaining, 0.2))
                self._apply_ready_locked()
            ready = self.version >= target
            return pb.WaitForVersionReply(
                ok=ready,
                code=OK if ready else VERSION_TIMEOUT,
                version=self.version,
                takeover_in_progress=self.takeover_active,
                message=(
                    "target version is available"
                    if ready
                    else "target version was not applied before the wait deadline"
                ),
            )

    def get_runtime(self, request: pb.RuntimeRequest) -> pb.RuntimeReply:
        timeout_ms = int(request.timeout_ms)
        deadline = time.monotonic() + max(0, timeout_ms) / 1000.0
        with self.condition:
            while True:
                assignments = self._runtime_assignments_locked()
                all_owned_by_active = bool(assignments) and all(
                    self.shards.get(owner, {}).get("status") == "active"
                    for owner, _generation in self.ownership.values()
                )
                ready = (
                    not self.takeover_active
                    and all_owned_by_active
                    and set(assignments.keys()) == set(
                        name for values in self.expected_assignments.values() for name in values
                    )
                )
                if ready or time.monotonic() >= deadline:
                    break
                self.condition.wait(timeout=0.1)
            records = []
            for shard_id in self.shard_ids:
                shard = self.shards.get(shard_id)
                if shard is None:
                    continue
                names = sorted(
                    name
                    for name, (owner, _generation) in self.ownership.items()
                    if owner == shard_id
                )
                generation = max(
                    [self.ownership[name][1] for name in names] or [int(shard["generation"])]
                )
                records.append(
                    pb.ShardAssignment(
                        shard_id=shard_id,
                        endpoint=shard["endpoint"],
                        parameter_names=names,
                        status=shard["status"],
                        generation=generation,
                    )
                )
            return pb.RuntimeReply(
                ok=ready,
                code=OK if ready else (TAKEOVER_IN_PROGRESS if self.takeover_active else NOT_READY),
                version=self.version,
                last_applied_step=self.last_applied_step,
                takeover_in_progress=self.takeover_active,
                shards=records,
            )

    def _runtime_assignments_locked(self) -> dict[str, str]:
        return {name: owner for name, (owner, _generation) in self.ownership.items()}

    def _refresh_liveness_locked(self, now: float) -> None:
        shards_to_take_over: list[str] = []
        for shard_id, record in self.shards.items():
            if record["status"] == "active" and (
                now - float(record["last_heartbeat"]) > self.shard_heartbeat_timeout
            ):
                record["status"] = "dead"
                record["last_error"] = "heartbeat timeout"
                self.connection.execute(
                    """
                    UPDATE shards
                    SET status = 'dead', last_error = 'heartbeat timeout'
                    WHERE shard_id = ? AND status = 'active'
                    """,
                    (shard_id,),
                )
                if any(owner == shard_id for owner, _generation in self.ownership.values()):
                    shards_to_take_over.append(shard_id)
        for worker_id, record in self.workers.items():
            if record["status"] == "active" and (
                now - float(record["last_heartbeat"]) > self.worker_heartbeat_timeout
            ):
                record["status"] = "offline"
                self.connection.execute(
                    "UPDATE workers SET status = 'offline' WHERE worker_id = ?",
                    (worker_id,),
                )
        for shard_id in shards_to_take_over:
            self._schedule_takeover_locked(shard_id)
        self._refresh_metrics_locked()

    def _refresh_metrics_locked(self) -> None:
        now = time.time()
        alive_workers = sum(
            1
            for record in self.workers.values()
            if record["status"] == "active"
            and now - float(record["last_heartbeat"]) <= self.worker_heartbeat_timeout
        )
        alive_shards = sum(
            1
            for record in self.shards.values()
            if record["status"] == "active"
            and now - float(record["last_heartbeat"]) <= self.shard_heartbeat_timeout
        )
        metrics.WORKERS_ALIVE.set(alive_workers)
        metrics.SHARDS_UP.set(alive_shards)
        for shard_id, record in self.shards.items():
            alive = (
                record["status"] == "active"
                and now - float(record["last_heartbeat"]) <= self.shard_heartbeat_timeout
            )
            metrics.SHARD_UP.labels(shard_id=shard_id).set(1 if alive else 0)
            for status in ("active", "dead", "unknown"):
                metrics.SHARD_STATUS.labels(shard_id=shard_id, status=status).set(0)
            metrics.SHARD_STATUS.labels(
                shard_id=shard_id, status=str(record["status"])
            ).set(1)
        metrics.PARAM_VERSION.set(self.version)
        metrics.PENDING_SUBMISSIONS.set(self.pending_count)
        metrics.TAKEOVER_IN_PROGRESS.set(1 if self.takeover_active else 0)

    def _initialize_metrics_from_db_locked(self) -> None:
        for result in ("applied", "queued", "duplicate", "stale", "rejected"):
            metrics.GRADIENT_SUBMISSIONS.labels(result=result).inc(0)
        for reason in ("duplicate", "stale_version"):
            metrics.GRADIENT_DISCARDED.labels(reason=reason).inc(0)
        for shard_id in self.shard_ids:
            metrics.SHARD_GRADIENT_SUBMISSIONS.labels(shard_id=shard_id).inc(0)
            metrics.STALE_REJECTIONS.labels(shard_id=shard_id).inc(0)
            for reason in ("duplicate", "stale_version"):
                metrics.SHARD_GRADIENT_DISCARDED.labels(
                    shard_id=shard_id, reason=reason
                ).inc(0)
        applied = self.connection.execute(
            "SELECT COUNT(*) AS count FROM submissions WHERE status = 'applied'"
        ).fetchone()["count"]
        duplicates = self.connection.execute(
            "SELECT COUNT(*) AS count FROM duplicate_submissions"
        ).fetchone()["count"]
        stale = self.connection.execute(
            "SELECT COUNT(*) AS count FROM stale_rejections"
        ).fetchone()["count"]
        if applied:
            metrics.GRADIENT_SUBMISSIONS.labels(result="applied").inc(applied)
        if duplicates:
            metrics.GRADIENT_SUBMISSIONS.labels(result="duplicate").inc(duplicates)
            metrics.GRADIENT_DISCARDED.labels(reason="duplicate").inc(duplicates)
        if stale:
            metrics.GRADIENT_SUBMISSIONS.labels(result="stale").inc(stale)
            metrics.GRADIENT_DISCARDED.labels(reason="stale_version").inc(stale)
        for row in self.connection.execute(
            "SELECT shard_id, COUNT(DISTINCT step) AS count FROM routes GROUP BY shard_id"
        ):
            metrics.SHARD_GRADIENT_SUBMISSIONS.labels(
                shard_id=row["shard_id"]
            ).inc(int(row["count"]))
        route_shards = [
            row["shard_id"]
            for row in self.connection.execute(
                "SELECT DISTINCT shard_id FROM routes ORDER BY shard_id"
            )
        ]
        for shard_id in route_shards:
            metrics.STALE_REJECTIONS.labels(shard_id=shard_id).inc(stale)
        for row in self.connection.execute(
            "SELECT old_shard_id, new_shard_id FROM takeovers WHERE status = 'completed'"
        ):
            metrics.TAKEOVERS.labels(
                from_shard=row["old_shard_id"], to_shard=row["new_shard_id"]
            ).inc()

    def _monitor_loop(self) -> None:
        while not self.stop_event.is_set():
            try:
                with self.lock:
                    self._refresh_liveness_locked(time.time())
                    self._apply_ready_locked()
            except Exception:
                LOG.error("liveness monitor failed\n%s", traceback.format_exc())
            self.stop_event.wait(self.monitor_interval)

    def _schedule_takeover_locked(self, old_shard_id: str) -> None:
        if self.takeover_active:
            return
        now = time.time()
        if now - self.last_takeover_attempt < max(1.0, self.monitor_interval * 2):
            return
        old = self.shards.get(old_shard_id)
        if old is None or old["status"] != "dead":
            return
        candidates = [
            shard_id
            for shard_id, record in self.shards.items()
            if shard_id != old_shard_id
            and record["status"] == "active"
            and now - float(record["last_heartbeat"]) <= self.shard_heartbeat_timeout
        ]
        if not candidates:
            old["last_error"] = "no live parameter shard available for takeover"
            self.connection.execute(
                "UPDATE shards SET last_error = ? WHERE shard_id = ?",
                (old["last_error"], old_shard_id),
            )
            return
        target = sorted(candidates)[0]
        self.takeover_active = True
        self.last_takeover_attempt = now
        metrics.TAKEOVER_IN_PROGRESS.set(1)
        self.condition.notify_all()
        thread = threading.Thread(
            target=self._takeover_worker,
            args=(old_shard_id, target),
            name=f"takeover-{old_shard_id}-to-{target}",
            daemon=True,
        )
        thread.start()

    def _takeover_worker(self, old_shard_id: str, target_shard_id: str) -> None:
        event_id: int | None = None
        try:
            with self.lock:
                old = self.shards.get(old_shard_id)
                target = self.shards.get(target_shard_id)
                if (
                    old is None
                    or old["status"] != "dead"
                    or target is None
                    or target["status"] != "active"
                ):
                    self.takeover_active = False
                    metrics.TAKEOVER_IN_PROGRESS.set(0)
                    self.condition.notify_all()
                    return
                names = sorted(
                    name
                    for name, (owner, _generation) in self.ownership.items()
                    if owner == old_shard_id
                )
                generation = max(
                    [self.ownership[name][1] for name in names]
                    or [int(old["generation"])]
                ) + 1
                version = self.version
                parameter_payload = [
                    pb.Parameter(name=name, value=float(self.params[name]))
                    for name in names
                ]
                with db.transaction(self.connection):
                    cursor = self.connection.execute(
                        """
                        INSERT INTO takeovers(
                            old_shard_id, new_shard_id, parameter_names, trigger,
                            from_version, to_version, status, started_at
                        ) VALUES (?, ?, ?, 'heartbeat_timeout_or_process_exit', ?, NULL,
                                  'in_progress', ?)
                        """,
                        (
                            old_shard_id,
                            target_shard_id,
                            db.json_dumps_stable(names),
                            version,
                            time.time(),
                        ),
                    )
                    event_id = int(cursor.lastrowid)
                target_endpoint = target["endpoint"]
            channel = grpc.insecure_channel(
                target_endpoint,
                options=[
                    ("grpc.max_send_message_length", 16 * 1024 * 1024),
                    ("grpc.max_receive_message_length", 16 * 1024 * 1024),
                ],
            )
            try:
                grpc.channel_ready_future(channel).result(
                    timeout=self.takeover_rpc_timeout
                )
                stub = pb_grpc.ParameterShardStub(channel)
                reply = stub.InstallTakeover(
                    pb.InstallTakeoverRequest(
                        old_shard_id=old_shard_id,
                        target_shard_id=target_shard_id,
                        parameters=parameter_payload,
                        version=version,
                        generation=generation,
                    ),
                    timeout=self.takeover_rpc_timeout,
                )
            finally:
                channel.close()
            if not reply.ok:
                raise RuntimeError(f"{reply.code}: {reply.message}")
            completed_at = time.time()
            with self.lock:
                with db.transaction(self.connection):
                    for name in names:
                        self.connection.execute(
                            """
                            UPDATE shard_ownership
                            SET shard_id = ?, generation = ?
                            WHERE parameter_name = ? AND shard_id = ?
                            """,
                            (target_shard_id, generation, name, old_shard_id),
                        )
                        self.ownership[name] = (target_shard_id, generation)
                    self.connection.execute(
                        """
                        UPDATE takeovers
                        SET status = 'completed', to_version = ?, completed_at = ?
                        WHERE takeover_id = ?
                        """,
                        (version, completed_at, event_id),
                    )
                    self.connection.execute(
                        """
                        UPDATE shards
                        SET generation = ?, last_error = 'taken_over'
                        WHERE shard_id = ?
                        """,
                        (generation, old_shard_id),
                    )
                    self.connection.execute(
                        """
                        UPDATE shards
                        SET generation = ?, last_error = NULL
                        WHERE shard_id = ?
                        """,
                        (generation, target_shard_id),
                    )
                self.shards[old_shard_id]["last_error"] = "taken_over"
                self.shards[old_shard_id]["generation"] = generation
                self.shards[target_shard_id]["generation"] = generation
                self.takeover_active = False
                metrics.TAKEOVERS.labels(
                    from_shard=old_shard_id, to_shard=target_shard_id
                ).inc()
                metrics.TAKEOVER_IN_PROGRESS.set(0)
                self.condition.notify_all()
                LOG.warning(
                    "takeover completed old=%s new=%s version=%s names=%s event=%s",
                    old_shard_id,
                    target_shard_id,
                    version,
                    len(names),
                    event_id,
                )
                self._apply_ready_locked()
        except Exception as exc:
            LOG.error("takeover failed: %s\n%s", exc, traceback.format_exc())
            with self.lock:
                if event_id is not None:
                    self.connection.execute(
                        """
                        UPDATE takeovers
                        SET status = 'failed', error = ?, completed_at = ?
                        WHERE takeover_id = ?
                        """,
                        (str(exc)[:1000], time.time(), event_id),
                    )
                self.takeover_active = False
                metrics.TAKEOVER_IN_PROGRESS.set(0)
                self.condition.notify_all()

    def status_dict(self) -> dict[str, Any]:
        with self.lock:
            now = time.time()
            ownership_by_shard: dict[str, list[str]] = {
                shard_id: [] for shard_id in self.shard_ids
            }
            for name, (owner, _generation) in self.ownership.items():
                ownership_by_shard.setdefault(owner, []).append(name)
            shard_rows = []
            for shard_id in sorted(self.shards):
                record = self.shards[shard_id]
                shard_rows.append(
                    {
                        "declared_parameter_count": len(
                            record["declared_parameter_names"]
                        ),
                        "declared_parameter_names": sorted(
                            record["declared_parameter_names"]
                        ),
                        "endpoint": record["endpoint"],
                        "generation": int(record["generation"]),
                        "last_error": record["last_error"],
                        "last_heartbeat_age_ms": round(
                            max(0.0, now - float(record["last_heartbeat"])) * 1000.0,
                            3,
                        ),
                        "parameter_count": len(ownership_by_shard.get(shard_id, [])),
                        "parameter_names": sorted(
                            ownership_by_shard.get(shard_id, [])
                        ),
                        "shard_id": shard_id,
                        "status": record["status"],
                    }
                )
            worker_rows = []
            for worker_id in sorted(self.workers):
                record = self.workers[worker_id]
                worker_rows.append(
                    {
                        "consecutive_stale": int(record["consecutive_stale"]),
                        "data_shard_index": int(record["data_shard_index"]),
                        "lagging": bool(record["lagging"]),
                        "last_heartbeat_age_ms": round(
                            max(0.0, now - float(record["last_heartbeat"])) * 1000.0,
                            3,
                        ),
                        "local_step": int(record["local_step"]),
                        "sample_count": int(record["sample_count"]),
                        "status": record["status"],
                        "total_stale": int(record["total_stale"]),
                        "trigger_version_gap": int(record["trigger_version_gap"]),
                        "worker_id": worker_id,
                    }
                )
            takeover = self.connection.execute(
                "SELECT * FROM takeovers ORDER BY takeover_id DESC LIMIT 1"
            ).fetchone()
            takeover_record = None
            if takeover is not None:
                takeover_record = {
                    "completed_at": takeover["completed_at"],
                    "error": takeover["error"],
                    "from_version": int(takeover["from_version"]),
                    "new_shard_id": takeover["new_shard_id"],
                    "old_shard_id": takeover["old_shard_id"],
                    "parameter_count": len(json.loads(takeover["parameter_names"])),
                    "parameter_names": sorted(json.loads(takeover["parameter_names"])),
                    "started_at": takeover["started_at"],
                    "status": takeover["status"],
                    "to_version": takeover["to_version"],
                    "trigger": takeover["trigger"],
                    "takeover_id": int(takeover["takeover_id"]),
                }
            counts = {
                "applied_gradients": self.connection.execute(
                    "SELECT COUNT(*) AS count FROM submissions WHERE status = 'applied'"
                ).fetchone()["count"],
                "discarded_duplicates": self.connection.execute(
                    "SELECT COUNT(*) AS count FROM duplicate_submissions"
                ).fetchone()["count"],
                "stale_rejections": self.connection.execute(
                    "SELECT COUNT(*) AS count FROM stale_rejections"
                ).fetchone()["count"],
                "completed_steps": self.last_applied_step,
                "takeovers": self.connection.execute(
                    "SELECT COUNT(*) AS count FROM takeovers WHERE status = 'completed'"
                ).fetchone()["count"],
            }
            status = {
                "applied_version": self.version,
                "configured_steps": self.configured_steps,
                "counts": {key: int(value) for key, value in sorted(counts.items())},
                "last_applied_step": self.last_applied_step,
                "last_takeover": takeover_record,
                "pending_steps": len(self.pending),
                "pending_submissions": self.pending_count,
                "shards": shard_rows,
                "takeover_in_progress": bool(self.takeover_active),
                "workers": worker_rows,
            }
            return status

    def status_json(self) -> str:
        return db.json_dumps_stable(self.status_dict())


class CoordinatorServicer(pb_grpc.CoordinatorServicer):
    def __init__(self, coordinator: TrainingCoordinator):
        self.coordinator = coordinator

    def RegisterWorker(self, request, context):
        return self.coordinator.register_worker(request)

    def RegisterShard(self, request, context):
        return self.coordinator.register_shard(request)

    def Heartbeat(self, request, context):
        return self.coordinator.heartbeat(request)

    def SubmitGradient(self, request, context):
        started = time.perf_counter()
        try:
            return self.coordinator.submit_gradient(request)
        except Exception:
            LOG.error("submit failed\n%s", traceback.format_exc())
            return pb.GradientReply(
                ok=False,
                code=INTERNAL,
                message="coordinator failed to process gradient",
                status="rejected",
                latest_version=self.coordinator.version,
            )
        finally:
            metrics.GRADIENT_SUBMIT_DURATION.observe(time.perf_counter() - started)

    def GetRuntime(self, request, context):
        return self.coordinator.get_runtime(request)

    def WaitForVersion(self, request, context):
        return self.coordinator.wait_for_version(request)

    def GetStatus(self, request, context):
        return pb.StatusReply(json=self.coordinator.status_json())


def _serve_http(coordinator: TrainingCoordinator, host: str, port: int):
    from train.http_api import serve_status_http

    return serve_status_http(coordinator.status_json, host, port)


def serve(config_path: str = "config.json") -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    coordinator = TrainingCoordinator(config_path)
    pid_path = resolve_path("run/pids") / "coordinator.pid"
    pid_path.parent.mkdir(parents=True, exist_ok=True)
    pid_path.write_text(str(os.getpid()), encoding="ascii")
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=16),
        options=[
            ("grpc.max_send_message_length", 16 * 1024 * 1024),
            ("grpc.max_receive_message_length", 16 * 1024 * 1024),
        ],
    )
    pb_grpc.add_CoordinatorServicer_to_server(
        CoordinatorServicer(coordinator), server
    )
    address = (
        f"{coordinator.config['coordinator']['host']}:"
        f"{coordinator.config['coordinator']['port']}"
    )
    if server.add_insecure_port(address) == 0:
        raise RuntimeError(f"failed to bind gRPC coordinator at {address}")
    http_server = _serve_http(
        coordinator,
        coordinator.config["coordinator"]["host"],
        int(coordinator.config["coordinator"]["status_port"]),
    )
    coordinator.start()
    server.start()
    LOG.info(
        "coordinator listening grpc=%s status=http://%s:%s",
        address,
        coordinator.config["coordinator"]["host"],
        coordinator.config["coordinator"]["status_port"],
    )
    stopped = threading.Event()

    def stop(_signum=None, _frame=None):
        if not stopped.is_set():
            stopped.set()
            coordinator.stop_event.set()
            server.stop(grace=1.0).wait(timeout=3.0)
            http_server.shutdown()

    for signal_name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, signal_name):
            try:
                signal.signal(getattr(signal, signal_name), stop)
            except (ValueError, OSError):
                pass
    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        stop()
    finally:
        coordinator.close()
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the training coordinator")
    parser.add_argument("--config", default="config.json")
    args = parser.parse_args()
    return serve(args.config)


if __name__ == "__main__":
    raise SystemExit(main())
