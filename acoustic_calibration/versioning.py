"""Append-only calibration versions, hash-chain validation and diffs."""

from __future__ import annotations

import json
import os
import threading
import uuid
from dataclasses import replace
from pathlib import Path
from typing import Any

import numpy as np

from .errors import CalibrationError, E_RECOVERY_INVALID, E_VERSION_STALE
from .hashing import sha256_obj
from .models import CalibrationState
from .storage import atomic_write_json, exclusive_file_lock, read_json

VERSION_FILE_PATTERN = "v{version:010d}.json"
LATEST_FILE = "latest.json"


def _stale(message: str, **details: object) -> CalibrationError:
    return CalibrationError(E_VERSION_STALE, message, dict(details))


def _recovery_invalid(message: str, **details: object) -> CalibrationError:
    return CalibrationError(E_RECOVERY_INVALID, message, dict(details))


class StateStore:
    """Disk-backed immutable version store with an atomic latest pointer."""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.versions_dir = self.root / "versions"
        self.versions_dir.mkdir(parents=True, exist_ok=True)
        self.latest_path = self.root / LATEST_FILE
        self.trace_path = self.root / "trace.jsonl"
        self.lock_path = self.root / ".publish.lock"
        self._thread_lock = threading.RLock()

    def version_path(self, version: int) -> Path:
        return self.versions_dir / VERSION_FILE_PATTERN.format(version=int(version))

    def list_versions(self) -> list[int]:
        versions: list[int] = []
        for path in self.versions_dir.glob("v*.json"):
            stem = path.stem
            try:
                versions.append(int(stem[1:]))
            except ValueError:
                continue
        return sorted(versions)

    def load(self, version: int) -> CalibrationState:
        path = self.version_path(version)
        if not path.exists():
            raise _stale("calibration version does not exist", version=int(version))
        try:
            data = read_json(path)
            state = CalibrationState.from_dict(data)
        except Exception as exc:
            raise _stale("calibration version cannot be decoded", version=int(version), error=str(exc)) from exc
        if state.version != int(version):
            raise _stale(
                "version number does not match filename",
                filename_version=int(version),
                payload_version=state.version,
            )
        recomputed = state.compute_content_hash()
        if recomputed != state.content_hash:
            raise _stale(
                "published version content hash mismatch",
                version=state.version,
                expected=state.content_hash,
                actual=recomputed,
            )
        return state

    def latest(self) -> CalibrationState | None:
        with self._thread_lock:
            versions = self.list_versions()
            highest = self.load(versions[-1]) if versions else None
            if self.latest_path.exists():
                try:
                    pointer = read_json(self.latest_path)
                    version = int(pointer["version"])
                    expected_hash = str(pointer["content_hash"])
                    pointed = self.load(version)
                    if pointed.content_hash != expected_hash:
                        raise _stale(
                            "latest pointer hash does not match version file",
                            version=version,
                            expected=expected_hash,
                            actual=pointed.content_hash,
                        )
                    # A crash may occur after the immutable version rename but
                    # before the pointer rename. The newest complete version is
                    # still a valid publication and is therefore authoritative.
                    return highest if highest is not None else pointed
                except CalibrationError:
                    raise
                except Exception as exc:
                    raise _stale("latest pointer is invalid", error=str(exc)) from exc
            return highest

    def _append_trace(self, record: dict[str, Any]) -> None:
        self.trace_path.parent.mkdir(parents=True, exist_ok=True)
        encoded = json.dumps(record, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n"
        descriptor = os.open(self.trace_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
        try:
            os.write(descriptor, encoded.encode("utf-8"))
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def publish(
        self,
        state: CalibrationState,
        expected_latest_hash: str | None = None,
    ) -> CalibrationState:
        if state.compute_content_hash() != state.content_hash:
            raise _stale(
                "state content hash is not canonical",
                version=state.version,
                supplied=state.content_hash,
                actual=state.compute_content_hash(),
            )
        with self._thread_lock, exclusive_file_lock(self.lock_path):
            current = self.latest()
            current_hash = None if current is None else current.content_hash
            if expected_latest_hash is not None and current_hash != expected_latest_hash:
                raise _stale(
                    "expected parent version is stale",
                    expected_parent_hash=expected_latest_hash,
                    actual_parent_hash=current_hash,
                )
            expected_version = 1 if current is None else current.version + 1
            if state.version != expected_version:
                raise _stale(
                    "version must increase monotonically by one",
                    expected_version=expected_version,
                    supplied_version=state.version,
                )
            if state.parent_hash != current_hash:
                raise _stale(
                    "parent hash does not match the latest published version",
                    expected_parent_hash=current_hash,
                    supplied_parent_hash=state.parent_hash,
                )
            destination = self.version_path(state.version)
            if destination.exists():
                raise _stale("published versions are immutable", version=state.version)
            atomic_write_json(destination, state.to_dict(), mode=0o444)
            pointer = {
                "version": state.version,
                "content_hash": state.content_hash,
                "parent_hash": state.parent_hash,
            }
            atomic_write_json(self.latest_path, pointer, mode=0o444)
            self._append_trace(
                {
                    "event": "publish",
                    "version": state.version,
                    "content_hash": state.content_hash,
                    "parent_hash": state.parent_hash,
                    "affected_channels": list(state.affected_channels),
                    "affected_bands": list(state.affected_bands),
                    "run_id": state.run_id,
                }
            )
            return state

    def validate_chain(self) -> tuple[CalibrationState, ...]:
        versions = self.list_versions()
        states: list[CalibrationState] = []
        previous_hash: str | None = None
        previous_version = 0
        for version in versions:
            state = self.load(version)
            if state.version != previous_version + 1:
                raise _recovery_invalid(
                    "version sequence has a gap",
                    first_broken_version=state.version,
                    previous_version=previous_version,
                )
            if state.parent_hash != previous_hash:
                raise _recovery_invalid(
                    "version hash chain is broken",
                    first_broken_version=state.version,
                    expected_parent_hash=previous_hash,
                    actual_parent_hash=state.parent_hash,
                )
            states.append(state)
            previous_hash = state.content_hash
            previous_version = state.version
        return tuple(states)

    def rollback(
        self,
        target_version: int,
        expected_latest_hash: str | None = None,
    ) -> CalibrationState:
        target = self.load(target_version)
        latest = self.latest()
        if latest is None:
            raise _stale("no published version exists")
        if expected_latest_hash is not None and latest.content_hash != expected_latest_hash:
            raise _stale(
                "rollback parent is stale",
                expected_parent_hash=expected_latest_hash,
                actual_parent_hash=latest.content_hash,
            )
        # Rollback copies the target artifacts while appending a new immutable
        # version; nothing in the historical version files is rewritten.
        new_state = replace(
            target,
            version=latest.version + 1,
            parent_hash=latest.content_hash,
            rollback_of=target.version,
            run_id=f"rollback-{latest.version + 1}-{uuid.uuid4().hex[:8]}",
            affected_channels=tuple(range(len(target.channel_names))),
            affected_bands=tuple(range(len(target.bands))),
            recomputed_nodes=(),
        )
        new_state = new_state.with_computed_hash()
        return self.publish(new_state, expected_latest_hash=latest.content_hash)


def _flatten(data: Any, prefix: str = "") -> dict[str, Any]:
    if isinstance(data, dict):
        result: dict[str, Any] = {}
        for key, value in data.items():
            child = f"{prefix}.{key}" if prefix else str(key)
            result.update(_flatten(value, child))
        return result
    if isinstance(data, list):
        result = {}
        for index, value in enumerate(data):
            result.update(_flatten(value, f"{prefix}[{index}]"))
        return result
    return {prefix: data}


def version_diff(left: CalibrationState, right: CalibrationState) -> dict[str, Any]:
    left_params = _flatten(left.params.to_dict())
    right_params = _flatten(right.params.to_dict())
    param_changes: dict[str, dict[str, Any]] = {}
    for key in sorted(set(left_params) | set(right_params)):
        before = left_params.get(key)
        after = right_params.get(key)
        if before != after:
            param_changes[key] = {"old": before, "new": after}

    channel_changes: list[dict[str, Any]] = []
    for channel_index, (before, after) in enumerate(zip(left.channels, right.channels, strict=True)):
        old_coeff = np.asarray(before.filter.coefficients, dtype=np.float64)
        new_coeff = np.asarray(after.filter.coefficients, dtype=np.float64)
        if old_coeff.shape == new_coeff.shape:
            difference = new_coeff - old_coeff
            coefficient_rms = float(np.sqrt(np.mean(difference * difference)))
            coefficient_max = float(np.max(np.abs(difference)))
        else:
            coefficient_rms = float("inf")
            coefficient_max = float("inf")
        old_errors = {error.band_index: error.upper_db for error in before.errors}
        new_errors = {error.band_index: error.upper_db for error in after.errors}
        error_changes = {
            str(band): {"old": old_errors.get(band), "new": new_errors.get(band)}
            for band in sorted(set(old_errors) | set(new_errors))
            if old_errors.get(band) != new_errors.get(band)
        }
        channel_changes.append(
            {
                "channel_index": channel_index,
                "channel_name": after.alignment.channel_name,
                "arrival_changed": before.alignment.arrival_sample != after.alignment.arrival_sample,
                "old_arrival_sample": before.alignment.arrival_sample,
                "new_arrival_sample": after.alignment.arrival_sample,
                "coefficient_rms_difference": coefficient_rms,
                "coefficient_max_difference": coefficient_max,
                "error_changes": error_changes,
            }
        )

    return {
        "left_version": left.version,
        "right_version": right.version,
        "left_hash": left.content_hash,
        "right_hash": right.content_hash,
        "left_is_direct_parent": right.parent_hash == left.content_hash,
        "param_changes": param_changes,
        "affected_channels": list(right.affected_channels),
        "affected_bands": list(right.affected_bands),
        "channel_changes": channel_changes,
        "consistency_alerts": {
            "old_count": len(left.consistency_alerts),
            "new_count": len(right.consistency_alerts),
        },
    }
