from __future__ import annotations

import json
import os
import tempfile
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .canonical import canonical_json_bytes
from .errors import ErrorCode, HapticError
from .models import DeviceConfig, WaveSequence
from .sampler import (
    SegmentSignal,
    result_from_payload,
    result_to_payload,
)
from .versions import ConfigVersion, PublishedSignal

SNAPSHOT_SCHEMA_VERSION = 1


@dataclass(frozen=True, slots=True)
class PendingWork:
    target_publication_version: int
    base_publication_version: int | None
    parent_version_hash: str | None
    sequence: WaveSequence
    config_version: int
    config: DeviceConfig
    completed_signals: tuple[SegmentSignal, ...] = ()
    operation: str = "recompute"

    def with_signal(self, signal: SegmentSignal) -> PendingWork:
        merged = {existing.index: existing for existing in self.completed_signals}
        merged[signal.index] = signal
        return PendingWork(
            target_publication_version=self.target_publication_version,
            base_publication_version=self.base_publication_version,
            parent_version_hash=self.parent_version_hash,
            sequence=self.sequence,
            config_version=self.config_version,
            config=self.config,
            completed_signals=tuple(merged[index] for index in sorted(merged)),
            operation=self.operation,
        )

    def to_payload(self) -> dict[str, Any]:
        return {
            "target_publication_version": self.target_publication_version,
            "base_publication_version": self.base_publication_version,
            "parent_version_hash": self.parent_version_hash,
            "sequence": self.sequence.to_dict(),
            "config_version": self.config_version,
            "config": self.config.to_dict(),
            "completed_signals": [
                signal.to_payload(include_samples=True)
                for signal in self.completed_signals
            ],
            "operation": self.operation,
        }

    @classmethod
    def from_payload(cls, payload: Mapping[str, Any]) -> PendingWork:
        raw_target = (
            payload.get("target_publication_version")
            if isinstance(payload, Mapping)
            else None
        )
        try:
            sequence = WaveSequence.from_mapping(payload["sequence"])
            config = DeviceConfig.from_mapping(payload["config"])
            signals = tuple(
                SegmentSignal.from_payload(item)
                for item in payload.get("completed_signals", [])
            )
            pending = cls(
                target_publication_version=int(payload["target_publication_version"]),
                base_publication_version=(
                    None
                    if payload.get("base_publication_version") is None
                    else int(payload["base_publication_version"])
                ),
                parent_version_hash=(
                    None
                    if payload.get("parent_version_hash") is None
                    else str(payload["parent_version_hash"])
                ),
                sequence=sequence,
                config_version=int(payload["config_version"]),
                config=config,
                completed_signals=signals,
                operation=str(payload.get("operation", "recompute")),
            )
        except HapticError as exc:
            if exc.code == ErrorCode.RECOVERY_INVALID:
                if exc.version is None:
                    exc.version = int(raw_target) if raw_target is not None else 1
                raise
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "invalid pending work record",
                version=int(raw_target) if raw_target is not None else 1,
                field=exc.field,
                details={"cause": exc.to_dict()},
            ) from exc
        except (KeyError, TypeError, ValueError) as exc:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "invalid pending work record",
                version=int(raw_target) if raw_target is not None else 1,
                details={"error": str(exc)},
            ) from exc
        return pending


@dataclass(slots=True)
class SnapshotState:
    config_versions: tuple[ConfigVersion, ...] = ()
    publications: tuple[PublishedSignal, ...] = ()
    pending: PendingWork | None = None

    @property
    def next_publication_version(self) -> int:
        return (
            max((item.publication_version for item in self.publications), default=0) + 1
        )

    @property
    def chain_tip(self) -> str | None:
        return self.publications[-1].version_hash if self.publications else None

    def with_pending(self, pending: PendingWork | None) -> SnapshotState:
        return SnapshotState(
            config_versions=self.config_versions,
            publications=self.publications,
            pending=pending,
        )


def _published_to_payload(published: PublishedSignal) -> dict[str, Any]:
    return {
        **published.version_payload(),
        "version_hash": published.version_hash,
        "sequence": published.sequence.to_dict(),
        "config": published.config.to_dict(),
        "sampling": result_to_payload(published.sampling),
    }


def _published_from_payload(payload: Mapping[str, Any]) -> PublishedSignal:
    version = _record_version(payload)
    required = (
        "config_version",
        "sequence",
        "config",
        "sampling",
        "affected_segments",
        "recomputed_segments",
        "restored_from_checkpoint",
        "reused_segments",
        "affected_ranges",
        "reused_ranges",
        "recomputed_sample_count",
        "reused_sample_count",
        "full_recompute",
        "full_reason",
        "reasons",
        "version_hash",
    )
    for field_name in required:
        if field_name not in payload:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot version {version} is missing {field_name}",
                version=version,
                field=field_name,
            )
    try:
        sequence = WaveSequence.from_mapping(payload["sequence"])
        config = DeviceConfig.from_mapping(payload["config"])
        sampling = result_from_payload(payload["sampling"], sequence, config)
        ranges = tuple(
            (int(item[0]), int(item[1])) for item in payload["affected_ranges"]
        )
        reused_ranges = tuple(
            (int(item[0]), int(item[1])) for item in payload["reused_ranges"]
        )
        published = PublishedSignal(
            publication_version=version,
            config_version=int(payload["config_version"]),
            sequence=sequence,
            config=config,
            sampling=sampling,
            affected_segments=tuple(int(item) for item in payload["affected_segments"]),
            recomputed_segments=tuple(
                int(item) for item in payload["recomputed_segments"]
            ),
            restored_from_checkpoint=tuple(
                int(item) for item in payload["restored_from_checkpoint"]
            ),
            reused_segments=tuple(int(item) for item in payload["reused_segments"]),
            affected_ranges=ranges,
            reused_ranges=reused_ranges,
            recomputed_sample_count=int(payload["recomputed_sample_count"]),
            reused_sample_count=int(payload["reused_sample_count"]),
            full_recompute=bool(payload["full_recompute"]),
            full_reason=(
                None if payload["full_reason"] is None else str(payload["full_reason"])
            ),
            reasons=tuple(str(item) for item in payload["reasons"]),
            parent_version_hash=(
                None
                if payload.get("parent_version_hash") is None
                else str(payload["parent_version_hash"])
            ),
        )
    except HapticError as exc:
        if exc.code == ErrorCode.RECOVERY_INVALID:
            if exc.version is None:
                exc.version = version
            raise
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            f"snapshot version {version} contains invalid data",
            version=version,
            field=exc.field,
            details={"cause": exc.to_dict()},
        ) from exc
    except (KeyError, TypeError, ValueError, IndexError) as exc:
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            f"snapshot version {version} is malformed",
            version=version,
            details={"error": str(exc)},
        ) from exc
    expected_hash = str(payload["version_hash"])
    if published.version_hash != expected_hash:
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            f"snapshot version {version} hash mismatch",
            version=version,
            field="version_hash",
        )
    return published


def _record_version(payload: Mapping[str, Any]) -> int:
    try:
        version = int(payload["publication_version"])
    except (KeyError, TypeError, ValueError) as exc:
        raise HapticError(
            ErrorCode.RECOVERY_INVALID,
            "snapshot publication is missing a valid publication_version",
            version=1,
            field="publication_version",
        ) from exc
    return version


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        try:
            directory_fd = os.open(path.parent, os.O_RDONLY)
        except OSError:
            directory_fd = None
        if directory_fd is not None:
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    except BaseException:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise


def _validate_config_chain(versions: tuple[ConfigVersion, ...]) -> None:
    previous_hash: str | None = None
    previous_version = 0
    for item in versions:
        if item.version != previous_version + 1:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "config version sequence is broken",
                version=item.version,
                field="version",
            )
        if item.parent_hash != previous_hash:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "config version hash chain is broken",
                version=item.version,
                field="parent_hash",
            )
        previous_hash = item.version_hash
        previous_version = item.version


def _validate_publication_chain(publications: tuple[PublishedSignal, ...]) -> None:
    previous_hash: str | None = None
    previous_version = 0
    for item in publications:
        if item.publication_version != previous_version + 1:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "publication version sequence is broken",
                version=item.publication_version,
                field="publication_version",
            )
        if item.parent_version_hash != previous_hash:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "publication hash chain is broken",
                version=item.publication_version,
                field="parent_version_hash",
            )
        previous_hash = item.version_hash
        previous_version = item.publication_version


class SnapshotStore:
    def __init__(self, directory: str | os.PathLike[str]) -> None:
        self.directory = Path(directory)
        self.path = self.directory / "session.snapshot.json"

    def load(self) -> SnapshotState:
        if not self.path.exists():
            return SnapshotState()
        try:
            payload = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "snapshot file cannot be read",
                version=1,
                field="root",
                details={"error": str(exc)},
            ) from exc
        if not isinstance(payload, Mapping):
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "snapshot root must be an object",
                version=1,
                field="root",
            )
        if int(payload.get("schema_version", -1)) != SNAPSHOT_SCHEMA_VERSION:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "unsupported snapshot schema",
                version=1,
                field="schema_version",
            )
        raw_configs = payload.get("config_versions")
        raw_publications = payload.get("publications")
        if not isinstance(raw_configs, list) or not isinstance(raw_publications, list):
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "snapshot is missing version arrays",
                version=1,
                field="config_versions/publications",
            )
        config_versions_list: list[ConfigVersion] = []
        for index, item in enumerate(raw_configs, start=1):
            try:
                config_versions_list.append(ConfigVersion.from_payload(item))
            except HapticError as exc:
                if exc.version is None:
                    exc.version = index
                raise
        config_versions = tuple(config_versions_list)
        _validate_config_chain(config_versions)
        publications = tuple(_published_from_payload(item) for item in raw_publications)
        _validate_publication_chain(publications)
        for published in publications:
            matching_configs = [
                item
                for item in config_versions
                if item.version == published.config_version
            ]
            if not matching_configs:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "publication references a missing config version",
                    version=published.publication_version,
                    field="config_version",
                )
            if matching_configs[0].config.content_hash != published.config.content_hash:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "publication config does not match its config version",
                    version=published.publication_version,
                    field="config",
                )
        expected_tip = payload.get("chain_tip")
        actual_tip = publications[-1].version_hash if publications else None
        if expected_tip != actual_tip:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "snapshot chain tip mismatch",
                version=publications[-1].publication_version if publications else 1,
                field="chain_tip",
            )
        pending: PendingWork | None = None
        if payload.get("pending") is not None:
            pending = PendingWork.from_payload(payload["pending"])
            expected_target = (
                publications[-1].publication_version + 1 if publications else 1
            )
            if pending.target_publication_version != expected_target:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "pending work target version is not next in chain",
                    version=expected_target,
                    field="target_publication_version",
                )
            base_version = (
                publications[-1].publication_version if publications else None
            )
            if pending.base_publication_version != base_version:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "pending work base version is not current chain tip",
                    version=pending.target_publication_version,
                    field="base_publication_version",
                )
            if pending.parent_version_hash != actual_tip:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "pending work parent hash is broken",
                    version=pending.target_publication_version,
                    field="parent_version_hash",
                )
            pending_configs = [
                item
                for item in config_versions
                if item.version == pending.config_version
            ]
            if not pending_configs:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "pending work references a missing config version",
                    version=pending.target_publication_version,
                    field="config_version",
                )
            if pending_configs[0].config.content_hash != pending.config.content_hash:
                raise HapticError(
                    ErrorCode.RECOVERY_INVALID,
                    "pending config does not match config version",
                    version=pending.target_publication_version,
                    field="config",
                )
        return SnapshotState(
            config_versions=config_versions,
            publications=publications,
            pending=pending,
        )

    def save(self, state: SnapshotState) -> None:
        _validate_config_chain(state.config_versions)
        _validate_publication_chain(state.publications)
        payload = {
            "schema_version": SNAPSHOT_SCHEMA_VERSION,
            "chain_tip": state.chain_tip,
            "config_versions": [item.to_dict() for item in state.config_versions],
            "publications": [
                _published_to_payload(item) for item in state.publications
            ],
            "pending": None if state.pending is None else state.pending.to_payload(),
        }
        _atomic_write(
            self.path,
            canonical_json_bytes(payload) + b"\n",
        )

    def discard_temporary_files(self) -> None:
        if not self.directory.exists():
            return
        for path in self.directory.glob(f".{self.path.name}.*.tmp"):
            try:
                path.unlink()
            except OSError:
                pass


class AtomicTraceLog:
    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path)
        self._events: list[dict[str, Any]] = []
        if self.path.exists():
            for line in self.path.read_text(encoding="utf-8").splitlines():
                if not line.strip():
                    continue
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    break
                if isinstance(event, dict):
                    self._events.append(event)

    @property
    def events(self) -> tuple[dict[str, Any], ...]:
        return tuple(self._events)

    def append(self, event: Mapping[str, Any]) -> None:
        self._events.append(dict(event))
        data = b"\n".join(canonical_json_bytes(item) for item in self._events) + b"\n"
        _atomic_write(self.path, data)

    def replace_last(self, event: Mapping[str, Any]) -> None:
        if self._events:
            self._events[-1] = dict(event)
        else:
            self._events.append(dict(event))
        data = b"\n".join(canonical_json_bytes(item) for item in self._events) + b"\n"
        _atomic_write(self.path, data)


@dataclass(slots=True)
class RecoveryReport:
    status: str
    latest_publication: int | None
    pending_target: int | None = None
    restored_checkpoint_segments: tuple[int, ...] = ()
    recomputed_segments: tuple[int, ...] = ()
    recovered_publication: int | None = None
    note: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "event": "recovery",
            "status": self.status,
            "latest_publication": self.latest_publication,
            "pending_target": self.pending_target,
            "restored_checkpoint_segments": list(self.restored_checkpoint_segments),
            "recomputed_segments": list(self.recomputed_segments),
            "recovered_publication": self.recovered_publication,
            "note": self.note,
        }
