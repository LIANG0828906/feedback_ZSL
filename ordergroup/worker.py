from __future__ import annotations

import json
import signal
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


class ClientError(RuntimeError):
    def __init__(self, status: int, body: dict[str, Any]) -> None:
        self.status = status
        self.body = body
        error = body.get("error", {}) if isinstance(body, dict) else {}
        self.code = str(error.get("code", "HTTP_ERROR"))
        self.message = str(error.get("message", body))
        super().__init__(f"{self.code} (HTTP {status}): {self.message}")


class CoordinatorClient:
    def __init__(self, base_url: str, timeout_seconds: float = 10.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout_seconds = timeout_seconds

    def request(
        self, method: str, path: str, body: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(
                body, ensure_ascii=False, separators=(",", ":")
            ).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self.base_url + path,
            data=data,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(
                request, timeout=self.timeout_seconds
            ) as response:
                raw = response.read()
                return json.loads(raw.decode("utf-8")) if raw else {}
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                payload = json.loads(raw.decode("utf-8")) if raw else {}
            except (UnicodeDecodeError, json.JSONDecodeError):
                payload = {"raw": raw.decode("utf-8", errors="replace")}
            raise ClientError(exc.code, payload) from exc
        except (urllib.error.URLError, TimeoutError, ConnectionError) as exc:
            raise ClientError(0, {"error": {"code": "COORDINATOR_UNAVAILABLE", "message": str(exc)}}) from exc

    def join(
        self,
        member_id: str,
        session_timeout_ms: int,
        heartbeat_interval_ms: int,
    ) -> dict[str, Any]:
        return self.request(
            "POST",
            "/v1/members/join",
            {
                "member_id": member_id,
                "session_timeout_ms": session_timeout_ms,
                "heartbeat_interval_ms": heartbeat_interval_ms,
            },
        )

    def heartbeat(self, member_id: str) -> dict[str, Any]:
        return self.request(
            "POST", f"/v1/members/{_quote(member_id)}/heartbeat", {}
        )

    def leave(self, member_id: str) -> dict[str, Any]:
        return self.request(
            "POST", f"/v1/members/{_quote(member_id)}/leave", {}
        )

    def fetch(
        self,
        member_id: str,
        partition: int,
        max_records: int,
    ) -> dict[str, Any]:
        return self.request(
            "POST",
            f"/v1/members/{_quote(member_id)}/fetch",
            {"partition": partition, "max_records": max_records},
        )

    def commit(
        self,
        member_id: str,
        partition: int,
        offset: int,
        generation: int,
    ) -> dict[str, Any]:
        return self.request(
            "POST",
            f"/v1/members/{_quote(member_id)}/commit",
            {
                "partition": partition,
                "offset": offset,
                "generation": generation,
            },
        )

    def pause(self, member_id: str, partition: int) -> dict[str, Any]:
        return self.request(
            "POST",
            f"/v1/members/{_quote(member_id)}/pause",
            {"partition": partition},
        )

    def resume(self, member_id: str, partition: int) -> dict[str, Any]:
        return self.request(
            "POST",
            f"/v1/members/{_quote(member_id)}/resume",
            {"partition": partition},
        )

    def ack_revoke(
        self, member_id: str, partitions: list[int]
    ) -> dict[str, Any]:
        return self.request(
            "POST",
            f"/v1/members/{_quote(member_id)}/ack-revoke",
            {"partitions": partitions},
        )


def _quote(value: str) -> str:
    return urllib.parse.quote(value, safe="")


@dataclass
class WorkerOptions:
    coordinator_url: str = "http://127.0.0.1:8090"
    member_id: str = "worker-1"
    session_timeout_ms: int = 10000
    heartbeat_interval_ms: int = 1000
    batch_size: int = 250
    consume_delay_ms: int = 0
    stop_after_events: int = 0
    duration_seconds: float | None = None
    quiet: bool = False
    stop_heartbeat_after_ms: int | None = None
    pause_after_records: int | None = None
    pause_duration_ms: int = 3000
    pause_partition: int | None = None
    stall_after_records: int | None = None
    stall_duration_ms: int = 3000
    processed_log: str | None = None
    no_leave_on_exit: bool = False


@dataclass
class _RuntimeState:
    joined: bool = False
    active: bool = True
    generation: int = 0
    owned: set[int] = field(default_factory=set)
    paused: set[int] = field(default_factory=set)
    revoked: set[int] = field(default_factory=set)
    heartbeat_ok: int = 0
    heartbeat_errors: int = 0
    rejoin_count: int = 0
    last_heartbeat_at_ms: int = 0
    heartbeat_stopped: bool = False
    stop_requested: bool = False


class Worker:
    def __init__(self, options: WorkerOptions) -> None:
        self.options = options
        self.client = CoordinatorClient(options.coordinator_url)
        self.lock = threading.RLock()
        self.state = _RuntimeState()
        self._stop = threading.Event()
        self._heartbeat_thread: threading.Thread | None = None
        self._processed_total = 0
        self._processed_by_partition = {
            partition: 0 for partition in range(10000)
        }
        self._committed_batches = 0
        self._commit_errors = 0
        self._fetch_errors = 0
        self._pause_operations = 0
        self._seen_order_ids: set[str] = set()
        self._duplicate_events = 0
        self._started_monotonic = time.monotonic()
        self._started_at_ms = int(time.time() * 1000)
        self._processed_log_handle: Any = None
        self._last_progress_report = time.monotonic()

    def run(self) -> int:
        install_signal_handlers(self)
        if self.options.processed_log:
            path = Path(self.options.processed_log)
            path.parent.mkdir(parents=True, exist_ok=True)
            self._processed_log_handle = path.open(
                "a", encoding="utf-8", buffering=1
            )
        try:
            if not self._join_with_retry():
                return 2
            self._heartbeat_thread = threading.Thread(
                target=self._heartbeat_loop,
                name=f"heartbeat-{self.options.member_id}",
                daemon=True,
            )
            self._heartbeat_thread.start()
            self._consume_loop()
            return 0
        finally:
            self._stop.set()
            thread = self._heartbeat_thread
            if thread and thread.is_alive():
                thread.join(timeout=2.0)
            with self.lock:
                should_leave = (
                    self.state.active
                    and self.state.joined
                    and not self.state.heartbeat_stopped
                    and not self.options.no_leave_on_exit
                )
            if should_leave:
                try:
                    self.client.leave(self.options.member_id)
                except ClientError:
                    pass
            if self._processed_log_handle is not None:
                self._processed_log_handle.flush()
                self._processed_log_handle.close()
            self._print_summary()

    def _join_with_retry(self) -> bool:
        deadline = (
            self._started_monotonic + 30.0
            if self.options.duration_seconds is None
            else self._started_monotonic + self.options.duration_seconds + 30.0
        )
        while not self._stop.is_set() and time.monotonic() < deadline:
            try:
                result = self.client.join(
                    self.options.member_id,
                    self.options.session_timeout_ms,
                    self.options.heartbeat_interval_ms,
                )
            except ClientError as exc:
                if not self.options.quiet:
                    print(
                        json.dumps(
                            {
                                "event": "join_retry",
                                "member_id": self.options.member_id,
                                "error": str(exc),
                            },
                            ensure_ascii=False,
                        ),
                        flush=True,
                    )
                time.sleep(0.25)
                continue
            with self.lock:
                self.state.joined = True
                self.state.active = True
                self.state.generation = int(result.get("generation", 0))
                self.state.owned = set(result.get("owned_partitions", []))
                self.state.paused = set(result.get("paused_partitions", []))
                self.state.last_heartbeat_at_ms = int(time.time() * 1000)
            if not self.options.quiet:
                print(
                    json.dumps(
                        {
                            "event": "joined",
                            "member_id": self.options.member_id,
                            "generation": self.state.generation,
                            "owned_partitions": sorted(self.state.owned),
                        },
                        ensure_ascii=False,
                    ),
                    flush=True,
                )
            return True
        return False

    def _heartbeat_loop(self) -> None:
        interval = max(0.01, self.options.heartbeat_interval_ms / 1000.0)
        while not self._stop.wait(interval):
            fault_at = self.options.stop_heartbeat_after_ms
            elapsed_ms = (time.monotonic() - self._started_monotonic) * 1000
            if fault_at is not None and elapsed_ms >= fault_at:
                with self.lock:
                    first_report = not self.state.heartbeat_stopped
                    self.state.heartbeat_stopped = True
                if first_report and not self.options.quiet:
                    print(
                        json.dumps(
                            {
                                "event": "heartbeat_stopped",
                                "member_id": self.options.member_id,
                                "elapsed_ms": int(elapsed_ms),
                            }
                        ),
                        flush=True,
                    )
                continue
            try:
                result = self.client.heartbeat(self.options.member_id)
            except ClientError as exc:
                with self.lock:
                    self.state.heartbeat_errors += 1
                    if exc.code in ("MEMBER_NOT_ACTIVE", "UNKNOWN_MEMBER"):
                        self.state.active = False
                        self.state.owned.clear()
                        self.state.paused.clear()
                if exc.code in ("MEMBER_NOT_ACTIVE", "UNKNOWN_MEMBER"):
                    self._rejoin()
                continue
            revoked = set(int(item) for item in result.get("revoked_partitions", []))
            with self.lock:
                self.state.heartbeat_ok += 1
                self.state.last_heartbeat_at_ms = int(time.time() * 1000)
                self.state.generation = int(
                    result.get("generation", self.state.generation)
                )
                self.state.owned = set(result.get("owned_partitions", []))
                self.state.paused = set(result.get("paused_partitions", []))
                self.state.revoked = revoked
                self.state.active = bool(result.get("active", True))

    def _rejoin(self) -> None:
        with self.lock:
            self.state.rejoin_count += 1
            self.state.owned.clear()
            self.state.paused.clear()
            self.state.revoked.clear()
        try:
            result = self.client.join(
                self.options.member_id,
                self.options.session_timeout_ms,
                self.options.heartbeat_interval_ms,
            )
        except ClientError:
            return
        with self.lock:
            self.state.joined = True
            self.state.active = True
            self.state.generation = int(result.get("generation", 0))
            self.state.owned = set(result.get("owned_partitions", []))
            self.state.paused = set(result.get("paused_partitions", []))

    def _consume_loop(self) -> None:
        started = self._started_monotonic
        partition_cursor = 0
        pause_done = False
        stall_done = False
        while not self._stop.is_set():
            if self._deadline_reached(started):
                break
            with self.lock:
                active = self.state.active
                owned = sorted(self.state.owned)
                paused = set(self.state.paused)
                revoked = set(self.state.revoked)
                heartbeat_stopped = self.state.heartbeat_stopped
                stop_after = self.options.stop_after_events
            # Once heartbeats have deliberately stopped, do not fetch another
            # batch. The current batch was already committed before this loop.
            # The coordinator remains the sole authority for session expiry;
            # a temporarily delayed heartbeat thread must not self-evict.
            if heartbeat_stopped:
                with self.lock:
                    self.state.active = False
                break
            if revoked:
                # This path runs only after the current batch has been
                # processed and committed, so no partition can overlap.
                try:
                    self.client.ack_revoke(
                        self.options.member_id, sorted(revoked)
                    )
                    with self.lock:
                        self.state.revoked.difference_update(revoked)
                    if not self.options.quiet:
                        print(
                            json.dumps(
                                {
                                    "event": "revocation_acknowledged",
                                    "member_id": self.options.member_id,
                                    "partitions": sorted(revoked),
                                }
                            ),
                            flush=True,
                        )
                except ClientError:
                    pass
            if self.options.duration_seconds is not None and not active:
                time.sleep(0.05)
                continue
            candidates = [partition for partition in owned if partition not in paused]
            if not candidates:
                time.sleep(0.05)
                continue
            partition = candidates[partition_cursor % len(candidates)]
            partition_cursor += 1
            try:
                result = self.client.fetch(
                    self.options.member_id,
                    partition,
                    self.options.batch_size,
                )
            except ClientError as exc:
                self._fetch_errors += 1
                if exc.code in (
                    "MEMBER_NOT_ACTIVE",
                    "PARTITION_NOT_OWNED",
                    "REBALANCE_IN_PROGRESS",
                ):
                    time.sleep(0.05)
                else:
                    time.sleep(0.1)
                continue
            records = result.get("records", [])
            if not records:
                time.sleep(0.02)
                continue
            self._process_records(records)
            generation = int(result.get("generation", 0))
            next_offset = int(result.get("next_commit_offset") or 0)
            commit_result = self._commit_with_retry(
                partition, next_offset, generation
            )
            if commit_result == "committed":
                with self.lock:
                    self._committed_batches += 1
            elif commit_result in (
                "PARTITION_NOT_OWNED",
                "STALE_GENERATION",
                "MEMBER_NOT_ACTIVE",
            ):
                with self.lock:
                    self.state.owned.discard(partition)

            if (
                self.options.pause_after_records is not None
                and not pause_done
                and self._processed_total >= self.options.pause_after_records
            ):
                pause_done = self._run_pause_fault(partition)
            if (
                self.options.stall_after_records is not None
                and not stall_done
                and self._processed_total >= self.options.stall_after_records
            ):
                stall_done = True
                self._run_stall_fault()
            if stop_after and self._processed_total >= stop_after:
                break
            self._report_progress_if_due()

    def _commit_with_retry(
        self, partition: int, offset: int, generation: int
    ) -> str:
        transient = {
            "COORDINATOR_UNAVAILABLE",
            "INTERNAL_ERROR",
            "KAFKA_UNAVAILABLE",
            "BACKEND_ERROR",
            "HTTP_ERROR",
        }
        terminal = {
            "PARTITION_NOT_OWNED",
            "STALE_GENERATION",
            "MEMBER_NOT_ACTIVE",
            "UNKNOWN_MEMBER",
            "PARTITION_PAUSED",
            "OFFSET_REGRESSION",
            "INVALID_REQUEST",
        }
        delay = 0.1
        while not self._stop.is_set():
            try:
                self.client.commit(
                    self.options.member_id,
                    partition,
                    offset,
                    generation,
                )
                return "committed"
            except ClientError as exc:
                self._commit_errors += 1
                if exc.code in terminal:
                    return exc.code
                if exc.code not in transient and 400 <= exc.status < 500:
                    return exc.code
                time.sleep(delay)
                delay = min(2.0, delay * 2)
        return "STOPPED"

    def _deadline_reached(self, started: float) -> bool:
        return (
            self.options.duration_seconds is not None
            and time.monotonic() - started >= self.options.duration_seconds
        )

    def _process_records(self, records: list[dict[str, Any]]) -> None:
        for record in records:
            value = record.get("value")
            order_id = value.get("order_id") if isinstance(value, dict) else None
            if order_id is not None:
                if order_id in self._seen_order_ids:
                    self._duplicate_events += 1
                else:
                    self._seen_order_ids.add(order_id)
            if self.options.consume_delay_ms > 0:
                time.sleep(self.options.consume_delay_ms / 1000.0)
            partition = int(record.get("partition", -1))
            with self.lock:
                self._processed_total += 1
                self._processed_by_partition[partition] = (
                    self._processed_by_partition.get(partition, 0) + 1
                )
            if self._processed_log_handle is not None:
                self._processed_log_handle.write(
                    json.dumps(
                        {
                            "member_id": self.options.member_id,
                            "order_id": order_id,
                            "partition": partition,
                            "offset": record.get("offset"),
                            "processed_at_ms": int(time.time() * 1000),
                        },
                        ensure_ascii=False,
                        separators=(",", ":"),
                    )
                    + "\n"
                )

    def _run_pause_fault(self, current_partition: int) -> bool:
        partition = (
            self.options.pause_partition
            if self.options.pause_partition is not None
            else current_partition
        )
        with self.lock:
            owned = partition in self.state.owned
        if not owned:
            return False
        try:
            result = self.client.pause(self.options.member_id, partition)
        except ClientError:
            return False
        self._pause_operations += 1
        if not self.options.quiet:
            print(
                json.dumps(
                    {
                        "event": "partition_paused",
                        "member_id": self.options.member_id,
                        "partition": partition,
                        "committed_offset": result.get("committed_offset"),
                        "duration_ms": self.options.pause_duration_ms,
                    },
                    ensure_ascii=False,
                ),
                flush=True,
            )
        time.sleep(max(0, self.options.pause_duration_ms) / 1000.0)
        try:
            result = self.client.resume(self.options.member_id, partition)
        except ClientError:
            return False
        self._pause_operations += 1
        if not self.options.quiet:
            print(
                json.dumps(
                    {
                        "event": "partition_resumed",
                        "member_id": self.options.member_id,
                        "partition": partition,
                        "committed_offset": result.get("committed_offset"),
                    },
                    ensure_ascii=False,
                ),
                flush=True,
            )
        return True

    def _run_stall_fault(self) -> None:
        if not self.options.quiet:
            print(
                json.dumps(
                    {
                        "event": "consume_stall_started",
                        "member_id": self.options.member_id,
                        "duration_ms": self.options.stall_duration_ms,
                    }
                ),
                flush=True,
            )
        time.sleep(max(0, self.options.stall_duration_ms) / 1000.0)
        if not self.options.quiet:
            print(
                json.dumps(
                    {
                        "event": "consume_stall_finished",
                        "member_id": self.options.member_id,
                    }
                ),
                flush=True,
            )

    def _report_progress_if_due(self) -> None:
        now = time.monotonic()
        if now - self._last_progress_report < 5.0:
            return
        self._last_progress_report = now
        if self.options.quiet:
            return
        with self.lock:
            owned = sorted(self.state.owned)
            generation = self.state.generation
        print(
            json.dumps(
                {
                    "event": "progress",
                    "member_id": self.options.member_id,
                    "processed": self._processed_total,
                    "immediate_duplicates": self._duplicate_events,
                    "committed_batches": self._committed_batches,
                    "owned_partitions": owned,
                    "generation": generation,
                },
                ensure_ascii=False,
            ),
            flush=True,
        )

    def _print_summary(self) -> None:
        with self.lock:
            summary = {
                "event": "worker_summary",
                "member_id": self.options.member_id,
                "processed": self._processed_total,
                "processed_by_partition": {
                    str(partition): count
                    for partition, count in sorted(
                        self._processed_by_partition.items()
                    )
                    if count
                },
                "immediate_duplicates": self._duplicate_events,
                "committed_batches": self._committed_batches,
                "commit_errors": self._commit_errors,
                "fetch_errors": self._fetch_errors,
                "heartbeat_ok": self.state.heartbeat_ok,
                "heartbeat_errors": self.state.heartbeat_errors,
                "rejoin_count": self.state.rejoin_count,
                "pause_operations": self._pause_operations,
                "heartbeat_stopped": self.state.heartbeat_stopped,
                "duration_ms": int(
                    (time.monotonic() - self._started_monotonic) * 1000
                ),
            }
        print(json.dumps(summary, ensure_ascii=False), flush=True)


def load_fault_config(path: str, member_id: str) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    if not isinstance(data, dict):
        raise ValueError("fault configuration must be a JSON object")
    common = {
        key: value
        for key, value in data.items()
        if key not in ("members", "profiles")
    }
    members = data.get("members", {})
    if isinstance(members, dict):
        selected = members.get(member_id, members.get("*", {}))
    else:
        selected = {}
    if not isinstance(selected, dict):
        selected = {}
    return {**common, **selected}


def install_signal_handlers(worker: Worker) -> None:
    def _stop(_signum: int, _frame: object) -> None:
        worker._stop.set()  # noqa: SLF001

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(sig, _stop)
        except (ValueError, OSError):
            pass

