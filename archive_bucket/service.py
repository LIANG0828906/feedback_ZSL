"""Service orchestration, incremental synchronization, and verification."""

from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

from . import __version__
from .errors import BadRequest, BucketError, UpstreamError
from .generator import MiB, build_plan, payload_for, revision_size
from .storage import BucketStore


def _utcnow() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


class InjectedSyncFailure(BucketError):
    status = 503
    code = "injected_backup_write_failure"

    def __init__(self, message: str, **details: object) -> None:
        super().__init__(message, **details)


@dataclass
class FaultSettings:
    sync_pause: bool = False
    sync_pause_after: int = 0
    backup_write_failure_once: bool = False
    occupancy_limit_bytes: int = 200 * MiB
    gc_high_watermark_bytes: int = 190 * MiB
    gc_target_bytes: int = 180 * MiB
    auto_gc: bool = True

    def public(self) -> dict[str, Any]:
        return {
            "auto_gc": self.auto_gc,
            "backup_write_failure_once": self.backup_write_failure_once,
            "gc_high_watermark_bytes": self.gc_high_watermark_bytes,
            "gc_target_bytes": self.gc_target_bytes,
            "occupancy_limit_bytes": self.occupancy_limit_bytes,
            "sync_pause": self.sync_pause,
            "sync_pause_after": self.sync_pause_after,
        }


class FaultController:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._state = FaultSettings()

    def snapshot(self) -> FaultSettings:
        with self._lock:
            return FaultSettings(**self._state.__dict__)

    def public(self) -> dict[str, Any]:
        return self.snapshot().public()

    def update(self, values: Mapping[str, Any]) -> dict[str, Any]:
        with self._lock:
            state = self._state
            if "sync_pause" in values:
                state.sync_pause = bool(values["sync_pause"])
            if "sync_pause_after" in values:
                value = int(values["sync_pause_after"])
                if value < 0:
                    raise BadRequest("sync_pause_after must be >= 0")
                state.sync_pause_after = value
            if "backup_write_failure_once" in values:
                state.backup_write_failure_once = bool(values["backup_write_failure_once"])
            if "occupancy_limit_bytes" in values:
                limit = int(values["occupancy_limit_bytes"])
                if limit < 1:
                    raise BadRequest("occupancy_limit_bytes must be > 0")
                state.occupancy_limit_bytes = limit
                state.gc_high_watermark_bytes = max(1, int(limit * 0.95))
                state.gc_target_bytes = max(1, int(limit * 0.90))
            if "gc_high_watermark_bytes" in values:
                high = int(values["gc_high_watermark_bytes"])
                if high < 1:
                    raise BadRequest("gc_high_watermark_bytes must be > 0")
                state.gc_high_watermark_bytes = high
            if "gc_target_bytes" in values:
                target = int(values["gc_target_bytes"])
                if target < 1:
                    raise BadRequest("gc_target_bytes must be > 0")
                state.gc_target_bytes = target
            if "auto_gc" in values:
                state.auto_gc = bool(values["auto_gc"])
            return state.public()

    def check_sync_pause(self, applied_in_run: int) -> bool:
        with self._lock:
            if self._state.sync_pause:
                return True
            if (
                self._state.sync_pause_after
                and applied_in_run >= self._state.sync_pause_after
            ):
                self._state.sync_pause = True
                self._state.sync_pause_after = 0
                return True
            return False

    def consume_write_failure(self) -> bool:
        with self._lock:
            enabled = self._state.backup_write_failure_once
            if enabled:
                self._state.backup_write_failure_once = False
            return enabled


class MainClient:
    """Minimal HTTP client used by backup synchronization."""

    def __init__(self, base_url: str, timeout: float = 120.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def _request(self, path: str) -> tuple[int, bytes, Mapping[str, str]]:
        url = self.base_url + path
        request = urllib.request.Request(url, method="GET")
        try:
            with self._opener.open(request, timeout=self.timeout) as response:
                return response.status, response.read(), dict(response.headers.items())
        except urllib.error.HTTPError as exc:
            body = exc.read()
            message = f"source returned HTTP {exc.code}"
            try:
                parsed = json.loads(body.decode("utf-8"))
                message = parsed.get("message") or parsed.get("error", {}).get("message") or message
            except Exception:
                pass
            raise UpstreamError(
                message,
                source_status=exc.code,
                source_url_path=path,
            ) from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise UpstreamError(
                "source is unreachable",
                reason=str(exc),
                source_url_path=path,
            ) from exc

    def get_json(self, path: str) -> dict[str, Any]:
        status, body, _ = self._request(path)
        if status != 200:
            raise UpstreamError("unexpected source response", source_status=status)
        try:
            value = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise UpstreamError("source returned invalid JSON") from exc
        if not isinstance(value, dict):
            raise UpstreamError("source returned a non-object JSON value")
        return value

    def get_object_revision(self, key: str, revision_id: int) -> bytes:
        encoded_key = urllib.parse.quote(key, safe="")
        path = f"/v1/buckets/main/objects/{encoded_key}/revisions/{revision_id}"
        _, body, _ = self._request(path)
        return body


class ArchiveService:
    def __init__(
        self,
        root: str | Path,
        *,
        seed: int = 20261119,
        max_object_bytes: int = 64 * MiB,
    ) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.seed = seed
        self.max_object_bytes = max_object_bytes
        self.main = BucketStore("main", self.root / "main")
        self.backup = BucketStore("backup", self.root / "backup")
        self.faults = FaultController()
        self._sync_lock = threading.Lock()
        self.base_url: str | None = None
        self.started_monotonic = time.monotonic()
        self.started_at = _utcnow()

    def bucket(self, name: str) -> BucketStore:
        if name == "main":
            return self.main
        if name == "backup":
            return self.backup
        raise BadRequest("unknown bucket", bucket=name, allowed=["main", "backup"])

    def public_faults(self) -> dict[str, Any]:
        return {"faults": self.faults.public()}

    def update_faults(self, values: Mapping[str, Any]) -> dict[str, Any]:
        return {"faults": self.faults.update(values)}

    def maybe_auto_gc(self, bucket_name: str) -> dict[str, Any] | None:
        faults = self.faults.snapshot()
        if not faults.auto_gc:
            return None
        store = self.bucket(bucket_name)
        stats = store.stats()
        if stats["occupancy_bytes"] < faults.gc_high_watermark_bytes:
            return None
        if bucket_name == "main":
            protect_after = self.backup.applied_seq()
            protected: set[int] = set()
        else:
            protect_after = None
            protected = self.main.protected_revision_ids()
        result = store.gc(
            target_bytes=faults.gc_target_bytes,
            protect_after_seq=protect_after,
            protect_revision_ids=protected,
        )
        result["trigger"] = "occupancy_high_watermark"
        result["bucket"] = bucket_name
        return result

    def gc_bucket(
        self,
        bucket_name: str,
        *,
        target_bytes: int | None = None,
    ) -> dict[str, Any]:
        store = self.bucket(bucket_name)
        if bucket_name == "main":
            protect_after = self.backup.applied_seq()
            protected: set[int] = set()
        elif bucket_name == "backup":
            protect_after = None
            # Main snapshot references may be restored by a future rollback
            # commit. Keep those revisions available on the replica as well.
            protected = self.main.protected_revision_ids()
        else:
            raise BadRequest("unknown bucket", bucket=bucket_name)
        result = store.gc(
            target_bytes=target_bytes,
            protect_after_seq=protect_after,
            protect_revision_ids=protected,
        )
        result["bucket"] = bucket_name
        return result

    def sync(
        self,
        *,
        source: str | None = None,
        limit: int = 100,
        max_events: int = 10000,
    ) -> dict[str, Any]:
        with self._sync_lock:
            return self._sync_impl(
                source=source,
                limit=limit,
                max_events=max_events,
            )

    def _sync_impl(
        self,
        *,
        source: str | None = None,
        limit: int = 100,
        max_events: int = 10000,
    ) -> dict[str, Any]:
        source_url = (source or self.base_url or "").rstrip("/")
        if not source_url:
            raise BadRequest("sync source is required")
        limit = max(1, min(int(limit), 500))
        max_events = max(0, min(int(max_events), 100000))
        from_seq = self.backup.applied_seq()
        applied = 0
        paused = False
        latest_seq = from_seq
        client = MainClient(source_url)
        last_gc: dict[str, Any] | None = None

        if self.faults.snapshot().sync_pause:
            return {
                "applied_since_start": 0,
                "applied_seq": from_seq,
                "from_seq": from_seq,
                "lag_commits": max(0, self.main.latest_seq() - from_seq),
                "latest_source_seq": self.main.latest_seq(),
                "paused": True,
            }

        while applied < max_events:
            commits_path = (
                f"/v1/buckets/main/commits?after_seq={self.backup.applied_seq()}"
                f"&limit={limit}"
            )
            batch = client.get_json(commits_path)
            latest_seq = int(batch.get("latest_seq", latest_seq))
            events = batch.get("events") or []
            if not events:
                break
            for event in events:
                if self.faults.check_sync_pause(applied):
                    paused = True
                    break
                if self.faults.consume_write_failure():
                    raise InjectedSyncFailure(
                        "injected backup write failure",
                        applied_seq=self.backup.applied_seq(),
                        from_seq=from_seq,
                    )
                if event.get("op") == "put":
                    key = event.get("key")
                    revision_id = event.get("revision_id")
                    if not isinstance(key, str) or not isinstance(revision_id, int):
                        raise UpstreamError("source put event is malformed")
                    data = client.get_object_revision(key, revision_id)
                    self.backup.apply_sync_event(event, data)
                else:
                    self.backup.apply_sync_event(event)
                applied += 1
                if applied % 25 == 0:
                    last_gc = self.maybe_auto_gc("backup") or last_gc
            if paused or not batch.get("has_more"):
                break

        # Ask the source for its current head. This also covers an empty first
        # batch and keeps lag reporting meaningful after a concurrent write.
        try:
            tail = client.get_json(
                f"/v1/buckets/main/commits?after_seq={self.backup.applied_seq()}&limit=1"
            )
            latest_seq = max(latest_seq, int(tail.get("latest_seq", latest_seq)))
        except BucketError:
            pass
        last_gc = self.maybe_auto_gc("backup") or last_gc
        applied_seq = self.backup.applied_seq()
        return {
            "applied_seq": applied_seq,
            "applied_since_start": applied,
            "from_seq": from_seq,
            "lag_commits": max(0, latest_seq - applied_seq),
            "latest_source_seq": latest_seq,
            "paused": paused,
            "reclaimed": last_gc,
        }

    @staticmethod
    def _hash_or_stored(
        info: Mapping[str, Any] | None,
        verify_content: bool,
    ) -> str | None:
        if info is None:
            return None
        actual = info.get("actual_content_hash")
        if actual is not None:
            return actual
        if info.get("reclaimed") or not verify_content:
            return info.get("content_hash")
        return None

    @staticmethod
    def _content_missing(
        info: Mapping[str, Any] | None,
        verify_content: bool,
    ) -> bool:
        return bool(
            verify_content
            and info is not None
            and not info.get("reclaimed")
            and info.get("actual_content_hash") is None
        )

    def consistency_check(
        self,
        *,
        snapshot_id: str | None = None,
        commit_seq: int | None = None,
        verify_content: bool = True,
    ) -> dict[str, Any]:
        started_at = _utcnow()
        main_latest = self.main.latest_seq()
        backup_capture = self.backup.capture_state(
            None,
            use_current=True,
            verify_content=verify_content,
        )
        backup_applied = int(backup_capture["applied_seq"])
        backup_latest = int(backup_capture["latest_commit_seq"])
        snapshot_commit_seq: int | None = None
        if snapshot_id:
            snapshot = self.main.get_snapshot(snapshot_id)
            target_seq = int(snapshot["commit_seq"])
            snapshot_commit_seq = target_seq
            if commit_seq is not None:
                target_seq = min(target_seq, int(commit_seq))
        elif commit_seq is not None:
            target_seq = int(commit_seq)
        else:
            target_seq = main_latest
        if target_seq < 0 or target_seq > main_latest:
            raise BadRequest(
                "commit_seq is outside main bucket history",
                commit_seq=target_seq,
                main_latest_commit_seq=main_latest,
            )
        checked_seq = min(target_seq, backup_applied)
        main_capture = self.main.capture_state(
            checked_seq,
            use_current=(checked_seq == main_latest),
            verify_content=verify_content,
        )
        if checked_seq == backup_applied:
            # The captured state is the replica's actual start-time view. This
            # deliberately exposes local divergence instead of replaying only
            # the commits the replica claims to have applied.
            backup_state = backup_capture
        else:
            backup_state = self.backup.capture_state(
                checked_seq,
                use_current=False,
                verify_content=verify_content,
            )
        main_visible = main_capture["visible"]
        backup_visible = backup_state["visible"]
        main_hashes = main_capture["hashes"]
        backup_hashes = backup_state["hashes"]

        inconsistencies: list[dict[str, Any]] = []
        reason_counts = {
            "content_different": 0,
            "missing": 0,
            "revision_behind": 0,
        }
        for key in sorted(set(main_visible) | set(backup_visible)):
            main_revision = main_visible.get(key)
            backup_revision = backup_visible.get(key)
            main_info = main_hashes.get(main_revision) if main_revision is not None else None
            backup_info = (
                backup_hashes.get(backup_revision) if backup_revision is not None else None
            )
            if (
                main_revision is None
                or backup_revision is None
                or (main_revision is not None and main_info is None)
                or (backup_revision is not None and backup_info is None)
                or self._content_missing(main_info, verify_content)
                or self._content_missing(backup_info, verify_content)
            ):
                reason = "missing"
            elif main_revision != backup_revision:
                reason = "revision_behind"
            else:
                main_hash = self._hash_or_stored(main_info, verify_content)
                backup_hash = self._hash_or_stored(backup_info, verify_content)
                if main_hash == backup_hash and main_hash is not None:
                    continue
                reason = "content_different"
            reason_counts[reason] += 1
            item: dict[str, Any] = {
                "backup": (
                    {
                        "content_hash": self._hash_or_stored(backup_info, verify_content),
                        "revision_id": backup_revision,
                    }
                    if backup_revision is not None
                    else None
                ),
                "key": key,
                "main": (
                    {
                        "content_hash": self._hash_or_stored(main_info, verify_content),
                        "revision_id": main_revision,
                    }
                    if main_revision is not None
                    else None
                ),
                "reason": reason,
            }
            if reason == "revision_behind" and main_revision is not None and backup_revision is not None:
                item["direction"] = (
                    "backup_behind" if backup_revision < main_revision else "backup_ahead"
                )
            inconsistencies.append(item)

        result = {
            "backup_synced_commit_seq_at_start": backup_applied,
            "backup_latest_commit_seq_at_start": backup_latest,
            "checked_commit_seq": checked_seq,
            "compared_keys": len(set(main_visible) | set(backup_visible)),
            "consistent": not inconsistencies,
            "finished_at": _utcnow(),
            "inconsistencies": inconsistencies,
            "inconsistent_key_count": len(inconsistencies),
            "main_latest_commit_seq_at_start": main_latest,
            "reason_counts": reason_counts,
            "requested_commit_seq": target_seq,
            "snapshot_commit_seq": snapshot_commit_seq,
            "snapshot_id": snapshot_id,
            "started_at": started_at,
            "sync_lag_commits": max(0, main_latest - checked_seq),
            "verified_content": bool(verify_content),
        }
        self.main.set_meta("last_comparison", result)
        return result

    def latest_comparison(self) -> dict[str, Any] | None:
        value = self.main.get_meta("last_comparison", None)
        return value if isinstance(value, dict) else None

    def status(self) -> dict[str, Any]:
        main_stats = self.main.stats()
        backup_stats = self.backup.stats()
        main_latest = main_stats["latest_commit_seq"]
        backup_applied = backup_stats["applied_seq"]
        result = {
            "buckets": {
                "backup": {
                    **backup_stats,
                    "synced_commit_seq": backup_applied,
                },
                "main": {
                    **main_stats,
                    "synced_commit_seq": main_latest,
                },
            },
            "comparison": self.latest_comparison(),
            "config": {
                "gc_default_limit_bytes": self.faults.snapshot().occupancy_limit_bytes,
                "gc_high_watermark_bytes": self.faults.snapshot().gc_high_watermark_bytes,
                "gc_target_bytes": self.faults.snapshot().gc_target_bytes,
                "max_object_bytes": self.max_object_bytes,
                "seed": self.seed,
            },
            "service": "local-object-bucket",
            "started_at": self.started_at,
            "sync": {
                "applied_commit_seq": backup_applied,
                "caught_up": backup_applied == main_latest,
                "lag_commits": max(0, main_latest - backup_applied),
                "main_latest_commit_seq": main_latest,
            },
            "uptime_seconds": round(time.monotonic() - self.started_monotonic, 3),
            "version": __version__,
        }
        return result

    def metrics(self) -> str:
        status = self.status()
        comparison = status.get("comparison") or {}
        mismatch = int(comparison.get("inconsistent_key_count") or 0)
        lines = [
            "# HELP object_bucket_visible_objects Number of visible object keys.",
            "# TYPE object_bucket_visible_objects gauge",
            "# HELP object_bucket_occupancy_bytes Stored object bytes.",
            "# TYPE object_bucket_occupancy_bytes gauge",
            "# HELP object_bucket_latest_commit_seq Latest local commit sequence.",
            "# TYPE object_bucket_latest_commit_seq gauge",
            "# HELP object_bucket_synced_commit_seq Applied synchronization sequence.",
            "# TYPE object_bucket_synced_commit_seq gauge",
            "# HELP object_bucket_snapshots Number of snapshots.",
            "# TYPE object_bucket_snapshots gauge",
            "# HELP object_bucket_reclaimed_bytes Cumulative physically reclaimed bytes.",
            "# TYPE object_bucket_reclaimed_bytes counter",
            "# HELP object_bucket_reclaimed_revisions Number of reclaimed revisions.",
            "# TYPE object_bucket_reclaimed_revisions counter",
            "# HELP object_bucket_snapshot_referenced_revisions Distinct snapshot-referenced revisions.",
            "# TYPE object_bucket_snapshot_referenced_revisions gauge",
            "# HELP object_bucket_consistency_mismatch_keys Mismatched keys in the latest check.",
            "# TYPE object_bucket_consistency_mismatch_keys gauge",
            "# HELP object_bucket_sync_lag_commits Main commit sequence minus backup applied sequence.",
            "# TYPE object_bucket_sync_lag_commits gauge",
        ]
        for bucket_name in ("backup", "main"):
            bucket = status["buckets"][bucket_name]
            label = f'{{bucket="{bucket_name}"}}'
            lines.extend(
                [
                    f"object_bucket_visible_objects{label} {bucket['visible_object_count']}",
                    f"object_bucket_occupancy_bytes{label} {bucket['occupancy_bytes']}",
                    f"object_bucket_latest_commit_seq{label} {bucket['latest_commit_seq']}",
                    f"object_bucket_synced_commit_seq{label} {bucket['synced_commit_seq']}",
                    f"object_bucket_snapshots{label} {bucket['snapshot_count']}",
                    f"object_bucket_reclaimed_bytes{label} {bucket['reclaimed_bytes']}",
                    f"object_bucket_reclaimed_revisions{label} "
                    f"{bucket['reclaimed_revision_count']}",
                    f"object_bucket_snapshot_referenced_revisions{label} "
                    f"{bucket['snapshot_referenced_revisions']}",
                    f"object_bucket_consistency_mismatch_keys{label} {mismatch}",
                    f"object_bucket_sync_lag_commits{label} "
                    f"{status['sync']['lag_commits'] if bucket_name == 'backup' else 0}",
                ]
            )
        return "\n".join(lines) + "\n"

    def generate_dataset(
        self,
        *,
        seed: int | None = None,
        count: int = 200,
        target_bytes: int = 120 * MiB,
        revisions: int = 1,
        prefix: str = "archive",
        rate_per_second: float = 0.0,
    ) -> dict[str, Any]:
        seed = self.seed if seed is None else int(seed)
        revisions = max(1, min(int(revisions), 10))
        plan = build_plan(seed, int(count), int(target_bytes), prefix)
        writes = 0
        bytes_written = 0
        first_commit: int | None = None
        last_commit: int | None = None
        interval = 0.0 if rate_per_second <= 0 else 1.0 / rate_per_second
        next_write = time.monotonic()
        for revision in range(1, revisions + 1):
            for item in plan.objects:
                if interval:
                    delay = max(0.0, next_write - time.monotonic())
                    if delay:
                        time.sleep(delay)
                    next_write = max(time.monotonic(), next_write) + interval
                data = payload_for(seed, item, revision)
                result = self.main.put(item.key, data)
                writes += 1
                bytes_written += len(data)
                first_commit = result["commit_seq"] if first_commit is None else first_commit
                last_commit = result["commit_seq"]
                if writes % 20 == 0:
                    self.maybe_auto_gc("main")
        gc_result = self.maybe_auto_gc("main")
        return {
            "bytes_written": bytes_written,
            "first_commit_seq": first_commit,
            "last_commit_seq": last_commit,
            "object_count": plan.object_count,
            "planned_bytes": plan.total_bytes,
            "rate_per_second": rate_per_second,
            "revisions_per_object": revisions,
            "seed": seed,
            "writes": writes,
            "gc": gc_result,
        }
