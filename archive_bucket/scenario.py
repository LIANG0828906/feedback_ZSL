"""Deterministic end-to-end archival scenario and fault injection."""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests

from .generator import MiB, PlannedObject, build_plan, deterministic_payload, payload_for, revision_size


class ScenarioFailure(RuntimeError):
    pass


class RateGate:
    """Thread-safe operation pacing for configurable workload rates."""

    def __init__(self, operations_per_second: float) -> None:
        self.interval = 0.0 if operations_per_second <= 0 else 1.0 / operations_per_second
        self._lock = threading.Lock()
        self._next = time.monotonic()

    def wait(self) -> None:
        if self.interval <= 0:
            return
        with self._lock:
            now = time.monotonic()
            delay = max(0.0, self._next - now)
            self._next = max(now, self._next) + self.interval
        if delay:
            time.sleep(delay)


class Client:
    def __init__(self, base_url: str, timeout: float = 300.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.session = requests.Session()
        self.session.trust_env = False
        self.timeout = timeout

    def request(self, method: str, path: str, **kwargs: Any) -> requests.Response:
        response = self.session.request(
            method,
            self.base_url + path,
            timeout=self.timeout,
            **kwargs,
        )
        return response

    def json(self, method: str, path: str, **kwargs: Any) -> dict[str, Any]:
        response = self.request(method, path, **kwargs)
        if response.status_code >= 400:
            raise ScenarioFailure(
                f"{method} {path} failed with {response.status_code}: {response.text[:500]}"
            )
        value = response.json()
        if not isinstance(value, dict):
            raise ScenarioFailure(f"{method} {path} returned a non-object")
        return value

    def put(self, key: str, data: bytes) -> dict[str, Any]:
        path = "/v1/buckets/main/objects/" + requests.utils.quote(key, safe="")
        response = self.request("PUT", path, data=data)
        if response.status_code != 201:
            raise ScenarioFailure(
                f"PUT {key} failed with {response.status_code}: {response.text[:500]}"
            )
        return response.json()

    def get_bytes(self, path: str) -> requests.Response:
        return self.request("GET", path)


def _key_path(key: str) -> str:
    return "/v1/buckets/main/objects/" + requests.utils.quote(key, safe="")


def _revision_path(key: str, revision_id: int) -> str:
    return _key_path(key) + f"/revisions/{revision_id}"


def _assert_snapshot_matches_view(
    client: Client,
    snapshot: dict[str, Any],
) -> None:
    view = client.json(
        "GET",
        f"/v1/buckets/main/view?commit_seq={snapshot['commit_seq']}",
    )
    if view["visible"] != snapshot["visible"]:
        raise ScenarioFailure("snapshot coverage does not equal its commit-sequence view")


class Scenario:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.base_url = args.base_url.rstrip("/")
        self.client = Client(self.base_url, timeout=args.timeout)
        self.write_count = 0
        self.delete_count = 0
        self.snapshot_count = 0
        self.rollback_count = 0
        self.sync_runs = 0
        self.max_sync_lag_commits = 0
        self.auto_gc_events: list[dict[str, Any]] = []
        self.snapshots: dict[str, dict[str, Any]] = {}
        self.updated_revisions: dict[str, int] = {}
        self.initial_revisions: dict[str, int] = {}
        self.reclaimable_keys: list[str] = []
        self.reclaimable_revisions: list[tuple[str, int]] = []
        self.write_gate = RateGate(args.write_rate)
        self.delete_gate = RateGate(args.delete_rate)
        self.snapshot_gate = RateGate(args.snapshot_rate)
        self.plan = build_plan(
            args.seed,
            args.count,
            args.target_mib * MiB,
            args.prefix,
        )

    def run(self) -> dict[str, Any]:
        started_monotonic = time.monotonic()
        started_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        health = self.client.json("GET", "/v1/health")
        if health.get("status") != "ok":
            raise ScenarioFailure("service health check failed")

        self._load_initial_dataset()
        initial_snapshot = self._snapshot("initial")
        concurrent_snapshot = self._concurrent_snapshot_test()
        updated_snapshot = self._snapshot("updated")
        self._delete_objects(30, 10)
        temp_keys = self._prepare_reclaimable_history()
        sync_before_rollback = self._sync_with_faults()
        comparison_before_rollback = self.client.json(
            "POST", "/v1/consistency/check", json={}
        )
        snapshot_comparison = self.client.json(
            "POST",
            "/v1/consistency/check",
            json={"snapshot_id": updated_snapshot["snapshot_id"]},
        )
        if not comparison_before_rollback["consistent"]:
            raise ScenarioFailure("pre-rollback cross-bucket comparison found differences")
        if not snapshot_comparison["consistent"]:
            raise ScenarioFailure("snapshot cross-bucket comparison found differences")

        rollback_result = self._rollback_with_concurrent_readers(initial_snapshot)
        sync_after_rollback = self._sync()
        if sync_after_rollback["lag_commits"] != 0:
            raise ScenarioFailure("backup did not catch up after rollback")

        historical_revision = self._pick_historical_revision()
        gc_result = self._collect_garbage(historical_revision)
        final_comparison = self.client.json("POST", "/v1/consistency/check", json={})
        if not final_comparison["consistent"]:
            raise ScenarioFailure("final cross-bucket comparison found differences")

        status = self.client.json("GET", "/v1/status")
        metrics = self.client.request("GET", "/metrics").text
        if "object_bucket_occupancy_bytes{bucket=\"main\"}" not in metrics:
            raise ScenarioFailure("Prometheus metrics are missing the main bucket")
        if "object_bucket_sync_lag_commits{bucket=\"backup\"}" not in metrics:
            raise ScenarioFailure("Prometheus metrics are missing sync lag")

        result = {
            "auto_gc_triggered": bool(self.auto_gc_events),
            "consistency": final_comparison,
            "delete_count": self.delete_count,
            "duration_seconds": round(time.monotonic() - started_monotonic, 3),
            "faults": {
                "backup_write_failure_injected": True,
                "sync_pause_injected": True,
                "sync_pause_resumed": True,
            },
            "final_status": {
                "buckets": status["buckets"],
                "sync": status["sync"],
            },
            "gc": gc_result,
            "generated_bytes": self.plan.total_bytes,
            "generated_object_count": self.plan.object_count,
            "history_objects": {
                "count": len(temp_keys),
                "keys": temp_keys,
            },
            "prefix": self.args.prefix,
            "max_observed_sync_lag_commits": self.max_sync_lag_commits,
            "reclaimed_bytes": (
                status["buckets"]["main"]["reclaimed_bytes"]
                + status["buckets"]["backup"]["reclaimed_bytes"]
            ),
            "reclaimed_revisions": (
                status["buckets"]["main"]["reclaimed_revision_count"]
                + status["buckets"]["backup"]["reclaimed_revision_count"]
            ),
            "explicit_gc_reclaimed_bytes": (
                gc_result["main"]["reclaimed_bytes"]
                + gc_result["backup"]["reclaimed_bytes"]
            ),
            "explicit_gc_reclaimed_revisions": (
                gc_result["main"]["reclaimed_revision_count"]
                + gc_result["backup"]["reclaimed_revision_count"]
            ),
            "rollback_count": self.rollback_count,
            "rollback_result": rollback_result,
            "rates_per_second": {
                "deletes": self.args.delete_rate,
                "snapshots": self.args.snapshot_rate,
                "writes": self.args.write_rate,
            },
            "scenario": "archive-bucket-fault-recovery-v1",
            "seed": self.args.seed,
            "snapshot_comparison_consistent": snapshot_comparison["consistent"],
            "snapshot_count": self.snapshot_count,
            "snapshots": {
                name: {
                    "commit_seq": snapshot["commit_seq"],
                    "object_count": snapshot["object_count"],
                    "snapshot_id": snapshot["snapshot_id"],
                }
                for name, snapshot in self.snapshots.items()
            },
            "started_at": started_at,
            "sync_after_rollback": sync_after_rollback,
            "sync_before_rollback": sync_before_rollback,
            "sync_lag_commits": status["sync"]["lag_commits"],
            "sync_runs": self.sync_runs,
            "write_count": self.write_count,
        }
        return result

    def _load_initial_dataset(self) -> None:
        objects = self.plan.objects

        def worker(offset: int) -> tuple[int, int, list[tuple[str, int]]]:
            writes = 0
            total = 0
            revisions: list[tuple[str, int]] = []
            session = Client(self.base_url, timeout=self.args.timeout)
            for index in range(offset, len(objects), 2):
                item = objects[index]
                data = payload_for(self.args.seed, item, 1)
                self.write_gate.wait()
                path = "/v1/buckets/main/objects/" + requests.utils.quote(item.key, safe="")
                response = session.request("PUT", path, data=data)
                if response.status_code != 201:
                    raise ScenarioFailure(
                        f"initial PUT {item.key} failed: "
                        f"{response.status_code} {response.text[:300]}"
                    )
                writes += 1
                total += len(data)
                revisions.append((item.key, int(response.json()["revision_id"])))
            session.session.close()
            return writes, total, revisions

        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(worker, (0, 1)))
        self.write_count += sum(item[0] for item in results)
        loaded_bytes = sum(item[1] for item in results)
        for worker_result in results:
            self.initial_revisions.update(worker_result[2])
        if loaded_bytes != self.plan.total_bytes:
            raise ScenarioFailure("generated payload size differs from the plan")
        visible = self.client.json("GET", "/v1/buckets/main/objects?limit=1000")
        if len(visible["objects"]) < self.plan.object_count:
            raise ScenarioFailure("initial dataset is incomplete")

    def _snapshot(self, label: str) -> dict[str, Any]:
        self.snapshot_gate.wait()
        snapshot = self.client.json(
            "POST",
            "/v1/buckets/main/snapshots",
            json={"snapshot_id": f"{self.args.prefix.replace('/', '_')}_{label}_{int(time.time())}"},
        )
        self.snapshot_count += 1
        self.snapshots[label] = snapshot
        _assert_snapshot_matches_view(self.client, snapshot)
        return snapshot

    def _concurrent_snapshot_test(self) -> dict[str, Any]:
        chosen = self.plan.objects[: min(24, max(4, len(self.plan.objects) // 8))]

        def update_chunk(chunk: list[PlannedObject]) -> list[tuple[str, int]]:
            session = Client(self.base_url, timeout=self.args.timeout)
            revisions: list[tuple[str, int]] = []
            for item in chunk:
                data = payload_for(self.args.seed, item, 2)
                self.write_gate.wait()
                path = "/v1/buckets/main/objects/" + requests.utils.quote(item.key, safe="")
                response = session.request("PUT", path, data=data)
                if response.status_code != 201:
                    raise ScenarioFailure(f"update PUT failed: {response.status_code}")
                revisions.append((item.key, int(response.json()["revision_id"])))
            session.session.close()
            return revisions

        midpoint = len(chosen) // 2
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures = [
                pool.submit(update_chunk, chosen[:midpoint]),
                pool.submit(update_chunk, chosen[midpoint:]),
            ]
            time.sleep(0.002)
            self.snapshot_gate.wait()
            snapshot = self.client.json(
                "POST",
                "/v1/buckets/main/snapshots",
                json={"snapshot_id": f"{self.args.prefix.replace('/', '_')}_during_writes_{int(time.time())}"},
            )
            self.snapshot_count += 1
            self.snapshots["during_writes"] = snapshot
            update_results = [future.result() for future in futures]
            self.write_count += sum(len(item) for item in update_results)
            for chunk_result in update_results:
                self.updated_revisions.update(chunk_result)
        _assert_snapshot_matches_view(self.client, snapshot)

        first = chosen[0]
        old_revision = self.initial_revisions[first.key]
        old = self.client.get_bytes(_revision_path(first.key, old_revision))
        if old.status_code != 200 or old.content != payload_for(self.args.seed, first, 1):
            raise ScenarioFailure("old revision was not preserved after update")
        return snapshot

    def _delete_objects(self, offset: int, count: int) -> None:
        for item in self.plan.objects[offset : offset + count]:
            self.delete_gate.wait()
            response = self.client.request("DELETE", _key_path(item.key))
            if response.status_code != 200:
                raise ScenarioFailure(f"DELETE {item.key} failed: {response.status_code}")
            self.delete_count += 1
            self._record_auto_gc(response.json())

    def _prepare_reclaimable_history(self) -> list[str]:
        self.client.json(
            "POST",
            "/v1/faults",
            json={
                "auto_gc": True,
                "gc_high_watermark_bytes": 130 * MiB,
                "gc_target_bytes": 120 * MiB,
                "occupancy_limit_bytes": 160 * MiB,
            },
        )
        keys: list[str] = []
        for index in range(self.args.history_count):
            item = PlannedObject(
                f"{self.args.prefix}/_history/{index:03d}.bin",
                2 * MiB,
                "history",
            )
            keys.append(item.key)
            for revision in (1, 2):
                data = deterministic_payload(
                    self.args.seed,
                    item.key,
                    revision,
                    revision_size(item.size, revision),
                )
                self.write_gate.wait()
                result = self.client.put(item.key, data)
                self.write_count += 1
                if revision == 1:
                    self.reclaimable_revisions.append(
                        (item.key, int(result["revision_id"]))
                    )
                self._record_auto_gc(result)
        for key in keys:
            self.delete_gate.wait()
            response = self.client.request("DELETE", _key_path(key))
            if response.status_code != 200:
                raise ScenarioFailure(f"history DELETE failed: {response.status_code}")
            self.delete_count += 1
            self._record_auto_gc(response.json())
        self.reclaimable_keys = keys
        return keys

    def _record_auto_gc(self, result: dict[str, Any]) -> None:
        gc = result.get("gc")
        if gc:
            self.auto_gc_events.append(gc)

    def _sync(self, **overrides: Any) -> dict[str, Any]:
        body = {
            "source": self.base_url,
            "limit": 100,
            "max_events": 100000,
        }
        body.update(overrides)
        response = self.client.request("POST", "/v1/sync", json=body)
        self.sync_runs += 1
        if response.status_code != 200:
            raise ScenarioFailure(
                f"sync failed with {response.status_code}: {response.text[:500]}"
            )
        result = response.json()
        self.max_sync_lag_commits = max(
            self.max_sync_lag_commits,
            int(result.get("lag_commits", 0)),
        )
        return result

    def _sync_with_faults(self) -> dict[str, Any]:
        self.client.json(
            "POST",
            "/v1/faults",
            json={
                "sync_pause": False,
                "sync_pause_after": 7,
                "backup_write_failure_once": False,
            },
        )
        paused = self._sync()
        if not paused["paused"] or paused["applied_since_start"] != 7:
            raise ScenarioFailure("synchronization did not pause at the injected boundary")
        paused_applied = paused["applied_seq"]
        self.max_sync_lag_commits = max(
            self.max_sync_lag_commits,
            int(paused["lag_commits"]),
        )
        if paused["lag_commits"] <= 0:
            raise ScenarioFailure("paused synchronization unexpectedly caught up")

        self.client.json(
            "POST",
            "/v1/faults",
            json={
                "sync_pause": False,
                "sync_pause_after": 0,
                "backup_write_failure_once": True,
            },
        )
        before_failure = self.client.json(
            "GET", "/v1/buckets/backup/stats"
        )["applied_seq"]
        failure = self.client.request("POST", "/v1/sync", json={"source": self.base_url})
        self.sync_runs += 1
        if failure.status_code != 503:
            raise ScenarioFailure("injected backup write failure was not reported")
        after_failure = self.client.json(
            "GET", "/v1/buckets/backup/stats"
        )["applied_seq"]
        if before_failure != after_failure:
            raise ScenarioFailure("failed backup write advanced applied sequence")

        self.client.json(
            "POST",
            "/v1/faults",
            json={"sync_pause": False, "backup_write_failure_once": False},
        )
        resumed = self._sync()
        if resumed["lag_commits"] != 0:
            raise ScenarioFailure("backup did not catch up after injected failure")
        if resumed["applied_seq"] <= paused_applied:
            raise ScenarioFailure("resumed sync did not continue from its applied sequence")
        resumed["fault_recovery"] = {
            "applied_at_pause": paused_applied,
            "applied_after_failure": after_failure,
            "resumed": True,
        }
        return resumed

    def _rollback_with_concurrent_readers(
        self,
        snapshot: dict[str, Any],
    ) -> dict[str, Any]:
        before = self.client.json("GET", "/v1/buckets/main/view")
        stop = threading.Event()
        errors: list[str] = []
        observed: list[str] = []

        def reader(worker_id: int) -> None:
            client = Client(self.base_url, timeout=self.args.timeout)
            while not stop.is_set():
                try:
                    view = client.json("GET", "/v1/buckets/main/view")
                except Exception as exc:  # noqa: BLE001 - propagated to main thread
                    errors.append(f"reader {worker_id}: {exc}")
                    return
                observed.append(json.dumps(view["visible"], sort_keys=True))
                time.sleep(0.005)
            client.session.close()

        threads = [threading.Thread(target=reader, args=(index,), daemon=True) for index in range(2)]
        for thread in threads:
            thread.start()
        try:
            rollback = self.client.json(
                "POST",
                f"/v1/buckets/main/snapshots/{snapshot['snapshot_id']}/restore",
            )
            self.rollback_count += 1
            time.sleep(0.05)
        finally:
            stop.set()
            for thread in threads:
                thread.join(timeout=10)
        if errors:
            raise ScenarioFailure("; ".join(errors))
        after = self.client.json("GET", "/v1/buckets/main/view")
        if after["visible"] != snapshot["visible"]:
            raise ScenarioFailure("rollback did not restore the snapshot's visible set")
        valid = {
            json.dumps(before["visible"], sort_keys=True),
            json.dumps(after["visible"], sort_keys=True),
        }
        invalid = [value for value in observed if value not in valid]
        if invalid:
            raise ScenarioFailure("concurrent readers observed an intermediate rollback state")
        return {
            **rollback,
            "concurrent_reader_observations": len(observed),
            "intermediate_states_observed": 0,
        }

    def _pick_historical_revision(self) -> dict[str, Any]:
        for item in self.plan.objects:
            revision_id = self.updated_revisions.get(item.key)
            if revision_id is None:
                continue
            response = self.client.get_bytes(_revision_path(item.key, revision_id))
            if response.status_code != 200:
                raise ScenarioFailure("post-snapshot revision was not preserved as history")
            return {
                "key": item.key,
                "revision_id": revision_id,
                "content_hash": response.headers.get("X-Content-SHA256"),
            }
        raise ScenarioFailure("no post-snapshot revision was recorded")

    def _collect_garbage(self, retained: dict[str, Any]) -> dict[str, Any]:
        main_before = self.client.json("GET", "/v1/buckets/main/stats")["occupancy_bytes"]
        stop = threading.Event()
        read_errors: list[str] = []

        def reader() -> None:
            client = Client(self.base_url, timeout=self.args.timeout)
            while not stop.is_set():
                response = client.get_bytes(
                    _revision_path(retained["key"], retained["revision_id"])
                )
                if response.status_code != 200:
                    read_errors.append(f"retained revision read returned {response.status_code}")
                    return
            client.session.close()

        thread = threading.Thread(target=reader, daemon=True)
        thread.start()
        try:
            main_gc = self.client.json("POST", "/v1/buckets/main/gc", json={})
            backup_gc = self.client.json("POST", "/v1/buckets/backup/gc", json={})
        finally:
            stop.set()
            thread.join(timeout=10)
        if read_errors:
            raise ScenarioFailure("; ".join(read_errors))

        if not self.reclaimable_revisions:
            raise ScenarioFailure("no reclaimable history keys were recorded")
        reclaim_key, reclaim_revision = self.reclaimable_revisions[0]
        reclaimed_probe = self.client.get_bytes(
            _revision_path(reclaim_key, reclaim_revision)
        )
        if reclaimed_probe.status_code != 410:
            raise ScenarioFailure(
                "unreferenced historical revision was not reclaimed "
                f"(HTTP {reclaimed_probe.status_code})"
            )
        main_after = self.client.json("GET", "/v1/buckets/main/stats")["occupancy_bytes"]
        return {
            "backup": backup_gc,
            "main": main_gc,
            "main_after_bytes": main_after,
            "main_before_bytes": main_before,
            "retained_revision": retained,
        }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Run the archive bucket fault scenario")
    parser.add_argument("--base-url", default="http://127.0.0.1:8765")
    parser.add_argument("--seed", type=int, default=20261119)
    parser.add_argument("--count", type=int, default=200)
    parser.add_argument("--target-mib", type=int, default=120)
    parser.add_argument("--prefix", default=f"archive/run-{time.time_ns()}")
    parser.add_argument("--history-count", type=int, default=12)
    parser.add_argument(
        "--write-rate",
        type=float,
        default=0.0,
        help="PUT operations per second; 0 means unlimited",
    )
    parser.add_argument(
        "--delete-rate",
        type=float,
        default=0.0,
        help="DELETE operations per second; 0 means unlimited",
    )
    parser.add_argument(
        "--snapshot-rate",
        type=float,
        default=0.0,
        help="snapshot creations per second; 0 means unlimited",
    )
    parser.add_argument("--timeout", type=float, default=300.0)
    parser.add_argument("--output", default="artifacts/scenario_result.json")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        result = Scenario(args).run()
    except Exception as exc:  # noqa: BLE001 - CLI reports one concise failure
        print(f"SCENARIO FAILED: {type(exc).__name__}: {exc}", file=sys.stderr)
        if args.output:
            output = Path(args.output)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(
                json.dumps(
                    {
                        "error": f"{type(exc).__name__}: {exc}",
                        "scenario": "archive-bucket-fault-recovery-v1",
                        "status": "failed",
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
        return 1
    rendered = json.dumps(result, indent=2, sort_keys=True, ensure_ascii=False)
    print(rendered)
    if args.output:
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(rendered + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
