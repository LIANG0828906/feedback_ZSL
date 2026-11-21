"""Atomic versioned report publication and stale-version detection."""

from __future__ import annotations

import copy
import json
import threading
from dataclasses import dataclass
from pathlib import Path

from .errors import E_NOT_FOUND, E_VERSION_STALE, SyncError
from .models import content_hash
from .wavio import write_json_atomic


@dataclass(frozen=True)
class Snapshot:
    version: int
    content_hash: str
    payload: dict


def _report_digest(payload: dict) -> str:
    hashable = {key: value for key, value in payload.items() if key != "content_hash"}
    return content_hash(hashable)


class ResultStore:
    """A single in-process pointer to the last fully committed report.

    Reports are written first, then ``latest.json`` is atomically replaced. A
    crash can therefore leave an orphan report but never a partial latest report.
    """

    def __init__(self, root: str | Path):
        self.root = Path(root).expanduser().resolve()
        self.reports_dir = self.root / "reports"
        self.reports_dir.mkdir(parents=True, exist_ok=True)
        self.latest_path = self.reports_dir / "latest.json"
        self._lock = threading.RLock()
        self._current: Snapshot | None = None
        self.recover()

    @property
    def current(self) -> Snapshot | None:
        with self._lock:
            return self._current

    def recover(self) -> Snapshot | None:
        with self._lock:
            if not self.latest_path.exists():
                self._current = None
                return None
            try:
                pointer = json.loads(self.latest_path.read_text(encoding="utf-8"))
                report_path = self.root / pointer["report_file"]
                payload = json.loads(report_path.read_text(encoding="utf-8"))
                expected_hash = str(pointer["content_hash"])
                expected_version = int(pointer["version"])
            except (OSError, ValueError, KeyError, TypeError) as exc:
                raise SyncError(
                    E_VERSION_STALE,
                    f"committed report pointer is unreadable: {exc}",
                ) from exc
            actual_hash = _report_digest(payload)
            if (
                actual_hash != expected_hash
                or int(payload.get("version", -1)) != expected_version
                or payload.get("content_hash") != expected_hash
            ):
                raise SyncError(
                    E_VERSION_STALE,
                    "latest committed report failed its content-hash check",
                    {
                        "expected_hash": expected_hash,
                        "actual_hash": actual_hash,
                        "expected_version": expected_version,
                    },
                )
            self._current = Snapshot(
                version=expected_version,
                content_hash=expected_hash,
                payload=payload,
            )
            return self._snapshot_copy()

    def publish(self, payload: dict) -> Snapshot:
        with self._lock:
            current_version = self._current.version if self._current is not None else 0
            version = current_version + 1
            report = copy.deepcopy(payload)
            report["version"] = version
            report["previous_version"] = current_version if current_version else None
            digest = _report_digest(report)
            report["content_hash"] = digest
            filename = f"v{version:06d}-{digest[:12]}.json"
            report_path = self.reports_dir / filename
            write_json_atomic(report_path, report)
            write_json_atomic(
                self.latest_path,
                {
                    "version": version,
                    "content_hash": digest,
                    "report_file": str(report_path.relative_to(self.root)),
                },
            )
            snapshot = Snapshot(version=version, content_hash=digest, payload=report)
            self._current = snapshot
            return self._snapshot_copy()

    def get_result(self, version: int | None = None, content_hash_value: str | None = None) -> Snapshot:
        with self._lock:
            if self._current is None:
                raise SyncError(E_NOT_FOUND, "no committed synchronization result is available")
            current = self._current
            if version is not None and int(version) != current.version:
                raise SyncError(
                    E_VERSION_STALE,
                    f"requested version {version}, current version is {current.version}",
                    {"requested_version": int(version), "current_version": current.version},
                )
            if content_hash_value is not None and content_hash_value != current.content_hash:
                raise SyncError(
                    E_VERSION_STALE,
                    "requested content hash is stale",
                    {
                        "requested_hash": content_hash_value,
                        "current_hash": current.content_hash,
                    },
                )
            return self._snapshot_copy()

    def previous_delay_sequences(self, sample_rate: int) -> dict[int, list]:
        from .models import SegmentDelay

        with self._lock:
            if self._current is None:
                return {}
            payload = self._current.payload
        result: dict[int, list] = {}
        for channel_payload in payload.get("delay_estimation", {}).get("channels", []):
            try:
                channel = int(channel_payload["channel"])
                result[channel] = [
                    SegmentDelay(**item) for item in channel_payload.get("segments", [])
                ]
            except (KeyError, TypeError, ValueError):
                continue
        return result

    def _snapshot_copy(self) -> Snapshot:
        current = self._current
        if current is None:
            raise SyncError(E_NOT_FOUND, "no committed synchronization result is available")
        return Snapshot(
            version=current.version,
            content_hash=current.content_hash,
            payload=copy.deepcopy(current.payload),
        )
