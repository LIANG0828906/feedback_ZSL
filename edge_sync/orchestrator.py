"""Sync-service orchestrator: scheduling, switching and the HTTP API.

The scheduler keeps at most ``workload.task_concurrency`` running shards per
task and at most 16 in aggregate.  A shard is bound to its egress only at
claim time, so a switch changes not-yet-started shards without touching any
in-flight upload or any byte range already committed at the target.
"""

from __future__ import annotations

import http.server
import json
import threading
import time
import urllib.request

from .config import Config
from .health import HealthMonitor, Observation
from .metrics import build_report, render_prometheus
from .store import SyncStore
from .uploader import upload_shard
from .workload import ShardSpec, TaskSpec, payload_for


class EgressSemaphores:
    def __init__(self, config: Config):
        self._semaphores = {
            egress.egress_id: threading.Semaphore(egress.concurrency) for egress in config.egresses
        }

    def acquire(self, egress_id: str) -> None:
        self._semaphores[egress_id].acquire()

    def release(self, egress_id: str) -> None:
        self._semaphores[egress_id].release()


class Orchestrator:
    def __init__(
        self,
        config: Config,
        store: SyncStore,
        proxy_states: dict,
        proxy_addresses: dict[str, tuple[str, int]],
        target_address: tuple[str, int],
    ):
        self.config = config
        self.store = store
        self.proxy_states = proxy_states
        self.proxy_addresses = proxy_addresses
        self.target_address = target_address
        self.health = HealthMonitor(config)
        self.egress_locks = EgressSemaphores(config)
        self.global_slots = threading.Semaphore(config.total_concurrency)
        self.task_slots = {
            f"task-{index + 1:02d}": threading.Semaphore(config.workload.task_concurrency)
            for index in range(config.workload.tasks)
        }
        self.switch_counts: dict[str, int] = {egress.egress_id: 0 for egress in config.egresses}
        self.last_switch: dict | None = None
        self._stop = threading.Event()
        self._run_lock = threading.Lock()

    # ------------------------------------------------------------------ run
    def register_manifest(self, manifest: list[TaskSpec]) -> None:
        now_ms = int(time.time() * 1000)
        egress_ids = [egress.egress_id for egress in self.config.egresses]
        for task_index, task in enumerate(manifest):
            self.store.upsert_task(
                task.task_id, task.shard_count, task.total_bytes, now_ms, "registered"
            )
            planned = egress_ids[task_index % len(egress_ids)]
            for shard in task.shards:
                self.store.insert_shard(
                    {
                        "task_id": shard.task_id,
                        "shard_seq": shard.shard_seq,
                        "start_byte": shard.start_byte,
                        "end_byte": shard.end_byte,
                        "length": shard.length,
                        "checksum": shard.checksum,
                    },
                    planned,
                )

    def run(self, manifest: list[TaskSpec]) -> dict:
        with self._run_lock:
            self._stop.clear()
            self.register_manifest(manifest)
            threads = []
            for task in manifest:
                self.store.set_task_status(task.task_id, "running", int(time.time() * 1000))
                thread = threading.Thread(target=self._run_task, args=(task,), name=f"run-{task.task_id}")
                thread.start()
                threads.append(thread)
            for thread in threads:
                thread.join()
            counts = self.store.task_shard_counts()
            for task in manifest:
                status = "succeeded" if not counts.get(task.task_id, {}).get("failed") else "failed"
                self.store.set_task_status(task.task_id, status, int(time.time() * 1000))
            return self.snapshot()

    def _run_task(self, task: TaskSpec) -> None:
        task_semaphore = self.task_slots[task.task_id]
        workers: list[threading.Thread] = []
        while not self._stop.is_set():
            task_semaphore.acquire()
            if self._stop.is_set():
                task_semaphore.release()
                break
            self.global_slots.acquire()
            shard = self.store.claim_next(task.task_id, int(time.time() * 1000))
            if shard is None:
                self.global_slots.release()
                task_semaphore.release()
                break
            attempts = self._attempt_count(shard["task_id"], shard["shard_seq"])
            worker = threading.Thread(
                target=self._run_shard,
                args=(task, shard, task_semaphore, attempts),
                name=f"shard-{shard['task_id']}-{shard['shard_seq']}",
                daemon=True,
            )
            worker.start()
            workers.append(worker)
        for worker in workers:
            worker.join()

    def _attempt_count(self, task_id: str, shard_seq: int) -> int:
        rows = self.store.fetch_all(
            "SELECT COUNT(*) FROM attempts WHERE task_id=? AND shard_seq=?",
            (task_id, shard_seq),
        )
        return int(rows[0][0])

    def _run_shard(
        self,
        task: TaskSpec,
        shard: dict,
        task_semaphore: threading.Semaphore,
        attempts: int,
    ) -> None:
        last_error = None
        used_egress = shard["planned_egress"]
        try:
            while attempts < self.config.workload.shard_max_attempts and not self._stop.is_set():
                egress_id = self._select_egress(shard)
                used_egress = egress_id
                self.egress_locks.acquire(egress_id)
                try:
                    result = self._attempt_upload(task, shard, egress_id, attempts + 1)
                finally:
                    self.egress_locks.release(egress_id)
                attempts += 1
                last_error = result.error_code
                self._observe(result, egress_id)
                if result.success:
                    self.store.finish_shard(shard["task_id"], shard["shard_seq"], egress_id, True, None)
                    return
                if attempts >= self.config.workload.shard_max_attempts:
                    break
                # Failed attempt: requeue and let another claim pick it up on
                # a (possibly switched) healthy egress.
                self.store.requeue(shard["task_id"], shard["shard_seq"])
                time.sleep(min(1.0, 0.1 * attempts))
                shard = self.store.claim_next(task.task_id, int(time.time() * 1000))
                if shard is None:
                    return
            self.store.finish_shard(shard["task_id"], shard["shard_seq"], used_egress, False, last_error)
        except Exception as exc:  # defensive: never lose a shard silently
            self.store.finish_shard(
                shard["task_id"], shard["shard_seq"], used_egress, False, f"INTERNAL:{type(exc).__name__}"
            )
        finally:
            self.global_slots.release()
            task_semaphore.release()

    # -------------------------------------------------------------- attempt
    def _attempt_upload(self, task: TaskSpec, shard: dict, egress_id: str, attempt_no: int):
        spec = ShardSpec(
            task_id=shard["task_id"],
            shard_seq=shard["shard_seq"],
            start_byte=shard["start_byte"],
            end_byte=shard["end_byte"],
            length=shard["length"],
            checksum=shard["checksum"],
        )
        payload = payload_for(self.config, spec)
        started_ms = int(time.time() * 1000)
        result = upload_shard(
            self.proxy_addresses[egress_id],
            egress_id,
            shard,
            payload,
            self.config.workload.request_timeout_s,
        )
        ended_ms = int(time.time() * 1000)
        self.store.record_attempt(
            {
                "task_id": shard["task_id"],
                "shard_seq": shard["shard_seq"],
                "attempt_no": attempt_no,
                "egress": egress_id,
                "started_ms": started_ms,
                "ended_ms": ended_ms,
                "success": result.success,
                "duplicate": result.duplicate,
                "error_code": result.error_code,
                "signal": result.signal,
                "link": result.link,
            }
        )
        return result

    def _select_egress(self, shard: dict) -> str:
        planned = shard["planned_egress"]
        if self.health.is_healthy(planned):
            return planned
        target = self.health.choose_target(planned)
        return target

    def _observe(self, result, egress_id: str) -> None:
        link = result.link or {}
        rtt_median = link.get("rtt_median_ms")
        total_segments = int(link.get("total_segments", 0))
        retrans = int(link.get("retrans_segments", 0))
        retrans_rate = round(retrans / total_segments, 6) if total_segments else 0.0
        emulated_ms = float(link.get("emulated_ms", 0.0))
        observation = Observation(
            rtt_median_ms=float(rtt_median) if rtt_median is not None else None,
            retrans_rate=retrans_rate,
            emulated_ms=emulated_ms,
            wall_ms=int(time.time() * 1000),
        )
        trigger = self.health.observe(egress_id, observation)
        if trigger and trigger.get("reason_metric") != "recovered":
            self._apply_switch(trigger)
        elif trigger and trigger.get("reason_metric") == "recovered":
            self.health.monitors[egress_id].healthy = True

    def _apply_switch(self, trigger: dict) -> None:
        bad_egress = trigger["egress"]
        target = self.health.choose_target(bad_egress)
        count = self.store.update_planned_egress(bad_egress, target)
        # A trigger with no pending shard changes has no scheduling effect;
        # keep the egress marked unhealthy but do not emit a switch event.
        if count == 0:
            return
        event = {**trigger, "target_egress": target, "shard_count": count}
        event_seq = self.store.record_switch(event)
        self.switch_counts[bad_egress] += 1
        self.last_switch = {**event, "event_seq": event_seq}

    # --------------------------------------------------------------- status
    def snapshot(self) -> dict:
        task_counts = self.store.task_shard_counts()
        return {
            "healthy_egresses": self.health.healthy_egresses(),
            "egress_health": self.health.snapshot(),
            "tasks": task_counts,
            "last_switch": self.last_switch,
        }

    def prometheus(self) -> str:
        return render_prometheus(
            self.config,
            self.store,
            self.proxy_states,
            self.health.snapshot(),
            self.store.task_shard_counts(),
            self.switch_counts,
        )

    def report(self) -> dict:
        return build_report(self.config, self.store, self.proxy_states, self.health.snapshot())


class ServiceHandler(http.server.BaseHTTPRequestHandler):
    server_version = "EdgeSyncService/1.0"
    protocol_version = "HTTP/1.1"

    def _json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload, sort_keys=True).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        orchestrator: Orchestrator = self.server.orchestrator
        if self.path == "/status":
            self._json(200, orchestrator.snapshot())
        elif self.path == "/metrics":
            body = orchestrator.prometheus().encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/report":
            self._json(200, orchestrator.report())
        elif self.path == "/health":
            self._json(200, {"status": "ok"})
        else:
            self._json(404, {"error": "NOT_FOUND"})

    def log_message(self, *args) -> None:
        return


class ThreadingServiceServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def start_service(config: Config, orchestrator: Orchestrator) -> tuple[ThreadingServiceServer, threading.Thread]:
    server = ThreadingServiceServer((config.service_host, config.service_port), ServiceHandler)
    server.orchestrator = orchestrator  # type: ignore[attr-defined]
    thread = threading.Thread(target=server.serve_forever, name="service-http", daemon=True)
    thread.start()
    return server, thread


def http_get_json(url: str) -> dict:
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.loads(response.read().decode("utf-8"))
