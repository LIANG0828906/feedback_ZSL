from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request
from typing import Any

from .config import PlanConfig
from .errors import (
    INSTANCE_PRECHECK_FAILED,
    INSTANCE_RESTART_REFUSED,
    INSTANCE_STATS_FAILED,
    INSTANCE_SWITCH_FAILED,
    INSTANCE_UNAVAILABLE,
    ROLLBACK_VERIFICATION_FAILED,
    StateError,
)
from .sli import Sample, evaluate_samples
from .store import AuditStore, TERMINAL_BATCH_STATUSES, TERMINAL_RUN_STATUSES


class RolloutOrchestrator:
    def __init__(
        self,
        config: PlanConfig,
        store: AuditStore,
        instance_endpoints: dict[str, str] | None = None,
    ) -> None:
        self.config = config
        self.store = store
        self.instance_endpoints = instance_endpoints or {
            instance.instance_id: (
                f"http://{config.network.host}:{instance.port}"
            )
            for instance in config.instances
        }
        self.run_id = ""
        self.abort_event = threading.Event()
        self._abort_reason: str | None = None
        self._abort_lock = threading.Lock()
        self._run_lock = threading.Lock()
        self._current_batch_id: str | None = None
        self._last_snapshot: dict[str, Any] = {}

    def _batch_config(self, batch_id: str):
        return next(
            batch for batch in self.config.batches if batch.batch_id == batch_id
        )

    def _instance_config(self, instance_id: str):
        return next(
            instance
            for instance in self.config.instances
            if instance.instance_id == instance_id
        )

    def _endpoint(self, instance_id: str) -> str:
        return self.instance_endpoints[instance_id].rstrip("/")

    def _request(
        self,
        method: str,
        instance_id: str,
        path: str,
        payload: dict[str, Any] | None = None,
        timeout: float | None = None,
    ) -> tuple[int, dict[str, Any]]:
        data = None
        headers = {"Accept": "application/json"}
        if payload is not None:
            data = json.dumps(
                payload,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self._endpoint(instance_id) + path,
            data=data,
            method=method,
            headers=headers,
        )
        last_error: Exception | None = None
        for attempt in range(3):
            try:
                with urllib.request.urlopen(
                    request,
                    timeout=(
                        timeout
                        or self.config.network.request_timeout_seconds
                    ),
                ) as response:
                    body = response.read()
                    return response.status, json.loads(body.decode("utf-8"))
            except urllib.error.HTTPError as exc:
                body = exc.read()
                try:
                    parsed = json.loads(body.decode("utf-8"))
                except Exception:
                    parsed = {
                        "ok": False,
                        "code": "INSTANCE_UNAVAILABLE",
                        "message": str(exc),
                    }
                status = exc.code
                exc.close()
                return status, parsed
            except (
                OSError,
                urllib.error.URLError,
                json.JSONDecodeError,
            ) as exc:
                last_error = exc
                if attempt < 2:
                    time.sleep(0.05 * (attempt + 1))
        return 503, {
            "ok": False,
            "code": INSTANCE_UNAVAILABLE,
            "message": str(last_error) if last_error else "request failed",
            "instance_id": instance_id,
        }

    def run(self) -> dict[str, Any]:
        with self._run_lock:
            self.run_id = self.store.start_run(self.config)
            run = self.store.get_run(self.run_id)
            if run["status"] in TERMINAL_RUN_STATUSES:
                return self.store.status_summary(self.run_id)
            if run["status"] == "ABORTING":
                return self._complete_abort()
            return self._run_batches()

    def _run_batches(self) -> dict[str, Any]:
        while True:
            if self.abort_event.is_set():
                return self._complete_abort()
            batches = self.store.get_batches(self.run_id)
            batch = next(
                (
                    item
                    for item in batches
                    if item["status"] not in {"PASSED"}
                ),
                None,
            )
            if batch is None:
                self.store.finish_run(
                    self.run_id,
                    "RELEASED",
                    "all_batches_passed",
                    {"completed_batches": len(batches)},
                )
                return self.store.status_summary(self.run_id)
            status = batch["status"]
            if status in TERMINAL_BATCH_STATUSES:
                final = (
                    "ABORTED"
                    if status == "ABORTED"
                    else (
                        "PARTIAL_ROLLBACK"
                        if status == "PARTIAL_ROLLBACK"
                        else (
                            "ROLLBACK_FAILED"
                            if status == "ROLLBACK_FAILED"
                            else "ROLLED_BACK"
                        )
                    )
                )
                self.store.finish_run(
                    self.run_id,
                    final,
                    f"batch_{batch['batch_id']}_{status.lower()}",
                    {"terminal_batch": batch["batch_id"]},
                )
                return self.store.status_summary(self.run_id)

            self._current_batch_id = batch["batch_id"]
            if status in {"PENDING", "PRECHECKING", "SWITCHING"}:
                if status == "PENDING":
                    self.store.transition_batch(
                        self.run_id,
                        batch["batch_id"],
                        "PRECHECKING",
                        "PRECHECK_STARTED",
                        {"batch_id": batch["batch_id"]},
                    )
                self._switch_batch(batch["batch_id"])
                if self.abort_event.is_set():
                    return self._complete_abort()
                batch = self.store.get_batch(self.run_id, batch["batch_id"])
                if batch["status"] == "SWITCHING":
                    self._start_observation(batch["batch_id"])
                batch = self.store.get_batch(self.run_id, batch["batch_id"])

            if batch["status"] == "OBSERVING":
                abort = self._observe_batch(batch["batch_id"])
                if abort:
                    return self._complete_abort()
                batch = self.store.get_batch(self.run_id, batch["batch_id"])
                if batch["status"] != "OBSERVING":
                    continue
                self._decide_batch(batch["batch_id"])
                batch = self.store.get_batch(self.run_id, batch["batch_id"])

            if batch["status"] == "PASSED":
                continue
            if batch["status"] in {"ROLLING_BACK", "ABORTING"}:
                self._rollback_batch(batch["batch_id"])
                if self.abort_event.is_set():
                    return self._complete_abort()
                batch = self.store.get_batch(self.run_id, batch["batch_id"])
                if batch["status"] in TERMINAL_BATCH_STATUSES:
                    continue
            if self.abort_event.is_set():
                return self._complete_abort()
            if batch["status"] in TERMINAL_BATCH_STATUSES:
                continue
            raise StateError(
                "STATE_UNHANDLED_BATCH",
                f"unhandled batch status: {batch['status']}",
                {"batch_id": batch["batch_id"], "status": batch["status"]},
            )

    def _switch_batch(self, batch_id: str) -> None:
        batch = self.store.get_batch(self.run_id, batch_id)
        batch_config = self._batch_config(batch_id)
        switched = set(batch["switched_instances"])
        existing_precheck = batch.get("precheck", {})
        failures: list[dict[str, Any]] = list(
            existing_precheck.get("failed_instances", [])
        )
        ready: list[str] = []
        for instance_id in batch_config.instances:
            if self.abort_event.is_set():
                return
            if instance_id in switched:
                continue
            status_code, result = self._request(
                "POST",
                instance_id,
                "/admin/precheck",
                {"target_version": self.config.target_version},
            )
            if status_code == 200 and result.get("ready"):
                ready.append(instance_id)
                continue
            code = str(result.get("code", INSTANCE_PRECHECK_FAILED))
            if code not in {
                INSTANCE_PRECHECK_FAILED,
                INSTANCE_RESTART_REFUSED,
                INSTANCE_UNAVAILABLE,
            }:
                code = INSTANCE_PRECHECK_FAILED
            failure = {
                "instance_id": instance_id,
                "code": code,
                "reason": str(result.get("reason", "precheck_failed")),
                "http_status": status_code,
                "target_version": self.config.target_version,
            }
            failures.append(failure)
            self.store.mark_instance_error(
                self.run_id,
                batch_id,
                instance_id,
                code,
                {"stage": "precheck", **failure},
            )
        failures = self._dedupe_failures(failures)
        self.store.set_precheck(self.run_id, batch_id, ready, failures)

        for instance_id in ready:
            if self.abort_event.is_set():
                return
            status_code, result = self._request(
                "POST",
                instance_id,
                "/admin/activate",
                {"target_version": self.config.target_version},
            )
            if status_code == 200 and result.get("ok"):
                self.store.mark_instance_switched(
                    self.run_id,
                    batch_id,
                    instance_id,
                    self.config.target_version,
                    True,
                )
                continue
            code = str(result.get("code", INSTANCE_SWITCH_FAILED))
            if code not in {
                INSTANCE_SWITCH_FAILED,
                INSTANCE_UNAVAILABLE,
                INSTANCE_RESTART_REFUSED,
            }:
                code = INSTANCE_SWITCH_FAILED
            failure = {
                "instance_id": instance_id,
                "code": code,
                "reason": str(result.get("reason", "activate_failed")),
                "http_status": status_code,
                "target_version": self.config.target_version,
            }
            failures.append(failure)
            self.store.mark_instance_error(
                self.run_id,
                batch_id,
                instance_id,
                code,
                {"stage": "activate", **failure},
            )
            self._request(
                "POST",
                instance_id,
                "/admin/discard",
                {},
                timeout=1.0,
            )
        self.store.set_precheck(
            self.run_id,
            batch_id,
            ready,
            self._dedupe_failures(failures),
        )

    @staticmethod
    def _dedupe_failures(
        failures: list[dict[str, Any]]
    ) -> list[dict[str, Any]]:
        deduped: dict[str, dict[str, Any]] = {}
        for failure in failures:
            deduped[failure["instance_id"]] = failure
        return [deduped[key] for key in sorted(deduped)]

    def _start_observation(self, batch_id: str) -> None:
        batch = self.store.get_batch(self.run_id, batch_id)
        if batch["status"] == "OBSERVING":
            return
        started = time.time()
        ends = started + self.config.observations_seconds
        self.store.mark_observation_started(
            self.run_id, batch_id, started, ends
        )

    def _observe_batch(self, batch_id: str) -> bool:
        batch = self.store.get_batch(self.run_id, batch_id)
        if batch["status"] != "OBSERVING":
            return False
        ends_ts = float(batch["observation_ends_ts"])
        batch_config = self._batch_config(batch_id)
        cursors = self.store.max_sample_ids(self.run_id, batch_id)
        for instance_id in batch_config.instances:
            cursors.setdefault(instance_id, 0)
        next_interval = time.monotonic()
        while True:
            if self.abort_event.is_set():
                return True
            now = time.time()
            self._drain_batch_samples(batch_id, batch_config.instances, cursors)
            evaluation = self._evaluate_current(batch_id, batch)
            self._last_snapshot = evaluation["metrics"]
            self.store.persist_interval_snapshot(
                self.run_id, batch_id, evaluation["metrics"]
            )
            if now >= ends_ts:
                break
            next_interval += self.config.sampling.interval_seconds
            wait_seconds = min(
                max(0.0, next_interval - time.monotonic()),
                max(0.0, ends_ts - time.time()),
            )
            if wait_seconds <= 0:
                time.sleep(min(0.02, self.config.sampling.interval_seconds))
            elif self.abort_event.wait(wait_seconds):
                return True
        self._drain_batch_samples(batch_id, batch_config.instances, cursors)
        return self.abort_event.is_set()

    def _drain_batch_samples(
        self,
        batch_id: str,
        instance_ids: tuple[str, ...],
        cursors: dict[str, int],
    ) -> None:
        collected: list[dict[str, Any]] = []
        for instance_id in instance_ids:
            since = cursors.get(instance_id, 0)
            status_code, result = self._request(
                "GET",
                instance_id,
                f"/admin/stats?since={since}",
                timeout=min(2.0, self.config.network.request_timeout_seconds),
            )
            if status_code != 200 or not result.get("ok"):
                self.store.mark_instance_error(
                    self.run_id,
                    batch_id,
                    instance_id,
                    INSTANCE_STATS_FAILED,
                    {
                        "since": since,
                        "http_status": status_code,
                        "detail": result,
                    },
                )
                continue
            for sample in result.get("samples", []):
                sample_id = int(sample["sample_id"])
                if sample_id <= since:
                    continue
                collected.append(sample)
                cursors[instance_id] = max(cursors.get(instance_id, 0), sample_id)
        self.store.store_samples(self.run_id, batch_id, collected)

    def _evaluate_current(
        self,
        batch_id: str,
        batch: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        batch = batch or self.store.get_batch(self.run_id, batch_id)
        samples = [
            Sample(
                instance_id=row["instance_id"],
                sample_id=int(row["sample_id"]),
                status=int(row["status"]),
                latency_ms=float(row["latency_ms"]),
                version=row["version"],
            )
            for row in self.store.load_samples(self.run_id, batch_id)
        ]
        return evaluate_samples(
            self.config,
            samples,
            batch["switched_instances"],
        )

    def _decide_batch(self, batch_id: str) -> None:
        if self.abort_event.is_set():
            return
        batch = self.store.get_batch(self.run_id, batch_id)
        evaluation = self._evaluate_current(batch_id, batch)
        self._last_snapshot = evaluation["metrics"]
        if not batch["switched_instances"]:
            triggers = list(evaluation["triggers"])
            if not any(item["code"] == "NO_SWITCHED_INSTANCES" for item in triggers):
                triggers.insert(
                    0,
                    {
                        "metric": "switched_instance_count",
                        "observed": 0,
                        "threshold": 1,
                        "comparator": ">=",
                        "code": "NO_SWITCHED_INSTANCES",
                    },
                )
            evaluation["triggers"] = triggers
            evaluation["verdict"] = "FAIL"
            evaluation["reason"] = "no_switched_instances"
        target_status = (
            "PASSED" if evaluation["verdict"] == "PASS" else "ROLLING_BACK"
        )
        try:
            self.store.record_decision(
                self.run_id,
                batch_id,
                evaluation["verdict"],
                evaluation["reason"],
                evaluation["metrics"],
                evaluation["thresholds"],
                evaluation["triggers"],
                target_status,
            )
        except StateError:
            if self.abort_event.is_set():
                return
            raise

    def _rollback_batch(self, batch_id: str) -> None:
        batch = self.store.get_batch(self.run_id, batch_id)
        status = batch["status"]
        if status != "ROLLING_BACK":
            if status == "ABORTING":
                if batch["rollback_started_ts"] is None:
                    rollback_started = time.time()
                    self.store.transition_batch(
                        self.run_id,
                        batch_id,
                        "ABORTING",
                        "ROLLBACK_STARTED",
                        {"batch_id": batch_id, "reason": "manual_abort"},
                        extra_updates={
                            "rollback_started_ts": rollback_started
                        },
                    )
                else:
                    rollback_started = float(batch["rollback_started_ts"])
            else:
                rollback_started = time.time()
                self.store.transition_batch(
                    self.run_id,
                    batch_id,
                    "ROLLING_BACK",
                    "ROLLBACK_STARTED",
                    {"batch_id": batch_id},
                    extra_updates={"rollback_started_ts": rollback_started},
                )
        else:
            if batch["rollback_started_ts"] is None:
                rollback_started = time.time()
                self.store.transition_batch(
                    self.run_id,
                    batch_id,
                    "ROLLING_BACK",
                    "ROLLBACK_STARTED",
                    {"batch_id": batch_id},
                    extra_updates={"rollback_started_ts": rollback_started},
                )
            else:
                rollback_started = float(batch["rollback_started_ts"])
        batch_config = self._batch_config(batch_id)
        target = batch_config.rollback_version
        initial_switched = list(batch["switched_instances"])
        success_count = 0
        failure_count = 0
        for instance_id in initial_switched:
            if self.abort_event.is_set() and status != "ABORTING":
                break
            instance_state = self.store.get_instances(self.run_id).get(
                instance_id, {}
            )
            from_version = str(
                instance_state.get("active_version", self.config.target_version)
            )
            metrics = self._last_snapshot or self.store.latest_snapshot(
                self.run_id, batch_id
            )
            result: dict[str, Any] | None = None
            for attempt in range(2):
                result = self._rollback_instance_once(instance_id, target)
                result["attempts"] = attempt + 1
                if result["success"]:
                    break
                if result["code"] == INSTANCE_RESTART_REFUSED:
                    break
                if attempt == 0:
                    time.sleep(0.15)
            assert result is not None
            if not result["success"]:
                self.store.record_rollback_action(
                    self.run_id,
                    batch_id,
                    instance_id,
                    from_version,
                    target,
                    False,
                    result["code"],
                    result["detail"],
                    metrics,
                )
                failure_count += 1
                continue
            self.store.record_rollback_action(
                self.run_id,
                batch_id,
                instance_id,
                from_version,
                target,
                True,
                None,
                {
                    "stage": "verified",
                    "active_version": target,
                    "attempts": result["attempts"],
                },
                metrics,
            )
            success_count += 1

        refreshed = self.store.get_batch(self.run_id, batch_id)
        remaining = refreshed["switched_instances"]
        if not remaining:
            final_status = "ROLLED_BACK"
        elif success_count == 0:
            final_status = "ROLLBACK_FAILED"
        else:
            final_status = "PARTIAL_ROLLBACK"
        snapshot = self._last_snapshot or self.store.latest_snapshot(
            self.run_id, batch_id
        )
        snapshot = {
            **snapshot,
            "rollback_succeeded_instances": success_count,
            "rollback_failed_instances": failure_count,
            "remaining_switched_instances": remaining,
            "rollback_duration_ms": round(
                max(0.0, time.time() - rollback_started) * 1000.0, 3
            ),
        }
        self.store.finish_batch(
            self.run_id,
            batch_id,
            final_status,
            "rollback_failed" if failure_count else "rollback_complete",
            snapshot,
            set_rollback_finished=True,
        )

    def _rollback_instance_once(
        self,
        instance_id: str,
        target: str,
    ) -> dict[str, Any]:
        status_code, precheck = self._request(
            "POST",
            instance_id,
            "/admin/precheck",
            {"target_version": target, "mode": "rollback"},
        )
        if status_code != 200 or not precheck.get("ready"):
            code = str(precheck.get("code", INSTANCE_PRECHECK_FAILED))
            if code not in {
                INSTANCE_RESTART_REFUSED,
                INSTANCE_PRECHECK_FAILED,
                INSTANCE_UNAVAILABLE,
            }:
                code = INSTANCE_PRECHECK_FAILED
            return {
                "success": False,
                "code": code,
                "attempts": 1,
                "detail": {
                    "stage": "precheck",
                    "http_status": status_code,
                    "result": precheck,
                },
            }
        status_code, activation = self._request(
            "POST",
            instance_id,
            "/admin/activate",
            {"target_version": target},
        )
        if status_code != 200 or not activation.get("ok"):
            code = str(activation.get("code", INSTANCE_SWITCH_FAILED))
            if code not in {
                INSTANCE_SWITCH_FAILED,
                INSTANCE_UNAVAILABLE,
                INSTANCE_RESTART_REFUSED,
            }:
                code = INSTANCE_SWITCH_FAILED
            return {
                "success": False,
                "code": code,
                "attempts": 1,
                "detail": {
                    "stage": "activate",
                    "http_status": status_code,
                    "result": activation,
                },
            }
        status_code, instance_status = self._request(
            "GET", instance_id, "/admin/status"
        )
        verified = (
            status_code == 200
            and instance_status.get("active_version") == target
            and not instance_status.get("switched")
        )
        if not verified:
            return {
                "success": False,
                "code": ROLLBACK_VERIFICATION_FAILED,
                "attempts": 1,
                "detail": {
                    "stage": "verify",
                    "http_status": status_code,
                    "result": instance_status,
                },
            }
        return {
            "success": True,
            "code": None,
            "attempts": 1,
            "detail": {
                "stage": "verified",
                "active_version": target,
            },
        }

    def abort(self, reason: str) -> dict[str, Any]:
        if not reason or not reason.strip():
            reason = "manual_abort"
        with self._abort_lock:
            if not self.run_id:
                self.run_id = self.store.start_run(self.config)
            run = self.store.get_run(self.run_id)
            if run["status"] in TERMINAL_RUN_STATUSES:
                raise StateError(
                    "STATE_TERMINAL_RUN",
                    "run is already terminal",
                    {"run_id": self.run_id, "status": run["status"]},
                )
            batches = self.store.get_batches(self.run_id)
            current = next(
                (
                    item
                    for item in batches
                    if item["status"] not in TERMINAL_BATCH_STATUSES
                ),
                None,
            )
            batch_id = current["batch_id"] if current else None
            metrics = (
                self.store.latest_snapshot(self.run_id, batch_id)
                if batch_id
                else self._last_snapshot
            )
            self._abort_reason = reason.strip()
            self.store.record_manual_abort(
                self.run_id,
                batch_id,
                self._abort_reason,
                metrics,
            )
            self.abort_event.set()
            return {
                "ok": True,
                "run_id": self.run_id,
                "batch_id": batch_id,
                "reason": self._abort_reason,
            }

    def _complete_abort(self) -> dict[str, Any]:
        run = self.store.get_run(self.run_id)
        if run["status"] == "ABORTED":
            return self.store.status_summary(self.run_id)
        batches = self.store.get_batches(self.run_id)
        current = next(
            (
                item
                for item in batches
                if item["status"]
                not in {
                    "PASSED",
                    "ROLLED_BACK",
                    "PARTIAL_ROLLBACK",
                    "ROLLBACK_FAILED",
                    "ABORTED",
                }
            ),
            None,
        )
        if current is not None:
            batch_id = current["batch_id"]
            if current["switched_instances"]:
                self._rollback_batch(batch_id)
                batch = self.store.get_batch(self.run_id, batch_id)
                if batch["status"] not in {
                    "ROLLED_BACK",
                    "PARTIAL_ROLLBACK",
                    "ROLLBACK_FAILED",
                }:
                    if batch["status"] == "ABORTING":
                        self.store.finish_batch(
                            self.run_id,
                            batch_id,
                            "ABORTED",
                            "manual_abort",
                            self._last_snapshot,
                        )
            else:
                if current["status"] == "ABORTING":
                    self.store.finish_batch(
                        self.run_id,
                        batch_id,
                        "ABORTED",
                        "manual_abort",
                        self._last_snapshot,
                    )
        self.store.finish_run(
            self.run_id,
            "ABORTED",
            self._abort_reason or "manual_abort",
            self._last_snapshot,
        )
        return self.store.status_summary(self.run_id)

    def status(self) -> dict[str, Any]:
        if not self.run_id:
            self.run_id = self.store.start_run(self.config)
        return self.store.status_summary(self.run_id)
