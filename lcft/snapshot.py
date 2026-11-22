"""Atomic snapshots, hash-chain validation and restart recovery."""

from __future__ import annotations

import json
import threading
from copy import deepcopy
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .canonical import atomic_write_json, read_json, sha256_value
from .errors import LCFTError
from .hashchain import GENESIS, build_chain, validate_chain
from .params import LayoutParams


@dataclass(frozen=True)
class RecoveryState:
    report: dict[str, Any]
    precomputed_sentences: list[dict[str, Any]]
    precomputed_descriptions: list[dict[str, Any]]
    completed_ids: list[str]
    pending_sentence_ids: list[str]
    snapshot_version: int
    recovered_from_published_version: int
    checkpoint: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "recovered_from_published_version": self.recovered_from_published_version,
            "snapshot_version": self.snapshot_version,
            "checkpoint": self.checkpoint,
            "completed_ids": self.completed_ids,
            "pending_sentence_ids": self.pending_sentence_ids,
            "track_hash": self.report.get("track_hash"),
        }


def _recovery_error(
    message: str,
    *,
    field: str,
    index: int | None = None,
    identifier: str | None = None,
    reason: str | None = None,
    good_version: int | None = None,
) -> LCFTError:
    details: dict[str, Any] = {
        "first_broken_link": {"id": identifier, "reason": reason}
        if identifier is not None or reason is not None
        else None,
    }
    if index is not None:
        details["link_index"] = index
    if good_version is not None:
        details["recovered_from_good_snapshot_version"] = good_version
    return LCFTError("E_RECOVERY_INVALID", message, field=field, details=details)


class SnapshotStore:
    """A directory of immutable JSON snapshots plus one atomic latest pointer."""

    def __init__(self, directory: str | Path, track_id: str) -> None:
        self.directory = Path(directory)
        self.track_id = track_id
        self.directory.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._next_checkpoint = 1

    @property
    def latest_path(self) -> Path:
        return self.directory / "latest.json"

    def _published_path(self, version: int) -> Path:
        return self.directory / f"snapshot-v{version:06d}.json"

    def _checkpoint_path(self, version: int) -> Path:
        return self.directory / f"checkpoint-v{version:06d}.json"

    def _current_published_version(self) -> int | None:
        if not self.latest_path.exists():
            return None
        try:
            pointer = read_json(self.latest_path)
        except (OSError, ValueError):
            return None
        if pointer.get("snapshot_kind") == "checkpoint":
            return pointer.get("base_published_version")
        value = pointer.get("latest_version")
        return value if isinstance(value, int) else None

    def save_published(self, report: dict[str, Any]) -> Path:
        with self._lock:
            version = report.get("published_version")
            if not isinstance(version, int) or version < 1:
                raise ValueError("published report needs a positive integer published_version")
            snapshot = deepcopy(report)
            snapshot["snapshot_kind"] = "published"
            snapshot["snapshot_version"] = version
            snapshot["track_id"] = report.get("track_id", self.track_id)
            path = self._published_path(version)
            atomic_write_json(path, snapshot)
            atomic_write_json(
                self.latest_path,
                {
                    "schema_version": 1,
                    "track_id": self.track_id,
                    "latest_version": version,
                    "published_version": version,
                    "snapshot_kind": "published",
                    "snapshot_file": path.name,
                },
            )
            return path

    def save_checkpoint(
        self,
        base_report: dict[str, Any],
        *,
        precomputed_sentences: list[dict[str, Any]],
        precomputed_descriptions: list[dict[str, Any]],
        completed_ids: list[str],
        pending_sentence_ids: list[str],
        checkpoint_version: int | None = None,
    ) -> Path:
        expected_ids = [row.get("id") for row in base_report.get("sentences", [])]
        completed_count = len(completed_ids)
        if completed_ids != expected_ids[:completed_count]:
            raise _recovery_error(
                "checkpoint completed ids are not a stable source-order prefix",
                field="snapshot.completed_ids",
                reason="not_ordered_prefix",
            )
        if pending_sentence_ids != expected_ids[completed_count:]:
            raise _recovery_error(
                "checkpoint pending ids are not the remaining source-order suffix",
                field="snapshot.pending_sentence_ids",
                reason="not_ordered_suffix",
            )
        if [row.get("id") for row in precomputed_sentences] != completed_ids:
            raise _recovery_error(
                "checkpoint precomputed rows do not match completed ids",
                field="snapshot.precomputed_sentences",
                reason="row_id_mismatch",
            )
        with self._lock:
            if checkpoint_version is None:
                checkpoint_version = self._next_checkpoint
            self._next_checkpoint = max(self._next_checkpoint + 1, checkpoint_version + 1)
            sealed_sentences = [self._seal_estimate(row) for row in precomputed_sentences]
            sealed_descriptions = [
                self._seal_estimate(row) for row in precomputed_descriptions
            ]
            chain = build_chain(
                [("precompute-sentence", row) for row in sealed_sentences]
                + [
                    ("precompute-description", row)
                    for row in sealed_descriptions
                ]
            )
            state = {
                "schema_version": 1,
                "snapshot_kind": "checkpoint",
                "snapshot_version": checkpoint_version,
                "track_id": self.track_id,
                "base_published_version": base_report.get("published_version"),
                "base_track_hash": base_report.get("track_hash"),
                "param_version": base_report.get("param_version"),
                "params": deepcopy(base_report.get("params")),
                "params_hash": base_report.get("params_hash"),
                "precomputed_sentences": sealed_sentences,
                "precomputed_descriptions": sealed_descriptions,
                "completed_ids": completed_ids,
                "pending_sentence_ids": pending_sentence_ids,
                "chain": chain,
            }
            path = self._checkpoint_path(checkpoint_version)
            atomic_write_json(path, state)
            atomic_write_json(
                self.latest_path,
                {
                    "schema_version": 1,
                    "track_id": self.track_id,
                    "latest_version": checkpoint_version,
                    "published_version": base_report.get("published_version"),
                    "base_published_version": base_report.get("published_version"),
                    "snapshot_kind": "checkpoint",
                    "snapshot_file": path.name,
                },
            )
            return path

    @staticmethod
    def _seal_estimate(row: dict[str, Any]) -> dict[str, Any]:
        sealed = deepcopy(row)
        sealed["content_hash"] = sha256_value(
            {key: value for key, value in sealed.items() if key != "content_hash"}
        )
        return sealed

    def _find_good_published(self, exclude: int | None = None) -> int | None:
        versions: list[int] = []
        for path in self.directory.glob("snapshot-v*.json"):
            try:
                version = int(path.stem.split("-v", 1)[1])
            except (IndexError, ValueError):
                continue
            versions.append(version)
        for version in sorted(versions, reverse=True):
            if exclude is not None and version == exclude:
                continue
            try:
                snapshot = read_json(self._published_path(version))
                self._validate_published(snapshot)
            except (OSError, ValueError, LCFTError):
                continue
            return version
        return None

    def _validate_published(self, snapshot: Any) -> int:
        if not isinstance(snapshot, dict):
            raise _recovery_error(
                "snapshot root is not an object", field="snapshot"
            )
        required = (
            "schema_version",
            "snapshot_version",
            "published_version",
            "param_version",
            "params",
            "params_hash",
            "sentences",
            "descriptions",
            "speaker_marks",
            "trims",
            "contention_edges",
            "chain",
            "track_hash",
        )
        for key in required:
            if key not in snapshot:
                raise _recovery_error(
                    f"snapshot field {key} is missing",
                    field=f"snapshot.{key}",
                    reason="missing_field",
                )
        if snapshot.get("schema_version") != 1:
            raise _recovery_error(
                "unsupported snapshot schema version",
                field="snapshot.schema_version",
                reason="schema_version",
            )
        version = snapshot.get("snapshot_version")
        if not isinstance(version, int) or version < 1:
            raise _recovery_error(
                "snapshot version is invalid",
                field="snapshot.snapshot_version",
                reason="invalid_version",
            )
        if snapshot.get("published_version") != version:
            raise _recovery_error(
                "snapshot version and published version differ",
                field="snapshot.published_version",
                reason="version_mismatch",
            )
        for key in ("sentences", "descriptions", "speaker_marks", "trims", "contention_edges"):
            if not isinstance(snapshot.get(key), list):
                raise _recovery_error(
                    f"snapshot field {key} must be an array",
                    field=f"snapshot.{key}",
                    reason="invalid_type",
                )
        try:
            params = LayoutParams.from_dict(snapshot.get("params"))
        except LCFTError as exc:
            raise _recovery_error(
                "snapshot parameters are invalid",
                field="snapshot.params",
                reason=exc.code,
            ) from exc
        if snapshot.get("params_hash") != params.hash:
            raise _recovery_error(
                "parameter hash does not match snapshot parameters",
                field="snapshot.params_hash",
                reason="params_hash_mismatch",
            )
        validate_chain(snapshot.get("chain"), snapshot)
        expected_track_hash = sha256_value(
            {
                key: value
                for key, value in snapshot.items()
                if key not in ("track_hash", "snapshot_kind", "snapshot_version")
            }
        )
        if snapshot.get("track_hash") != expected_track_hash:
            raise _recovery_error(
                "track hash does not match snapshot contents",
                field="snapshot.track_hash",
                reason="track_hash_mismatch",
            )
        return version

    def _validate_precompute(self, snapshot: dict[str, Any]) -> None:
        if not isinstance(snapshot.get("precomputed_sentences"), list) or not isinstance(
            snapshot.get("precomputed_descriptions"), list
        ):
            raise _recovery_error(
                "checkpoint precomputed rows must be arrays",
                field="snapshot.precomputed_sentences",
                reason="invalid_type",
            )
        rows: list[tuple[str, dict[str, Any]]] = []
        for row in snapshot.get("precomputed_sentences", []):
            rows.append(("precompute-sentence", row))
        for row in snapshot.get("precomputed_descriptions", []):
            rows.append(("precompute-description", row))
        chain = snapshot.get("chain")
        if not isinstance(chain, list) or len(chain) != len(rows):
            raise _recovery_error(
                "checkpoint precompute chain length is invalid",
                field="snapshot.chain",
                reason="length_mismatch",
            )
        previous = GENESIS
        for index, ((label, row), link) in enumerate(zip(rows, chain)):
            if not isinstance(row, dict):
                raise _recovery_error(
                    f"checkpoint precompute row {index} is not an object",
                    field=f"snapshot.precomputed_rows[{index}]",
                    index=index,
                    reason="invalid_type",
                )
            identifier = row.get("id")
            reason = ""
            if not isinstance(link, dict) or any(
                key not in link for key in ("label", "id", "payload_hash", "prev", "hash")
            ):
                reason = "missing_field"
            elif link.get("label") != label or link.get("id") != identifier:
                reason = "identity_mismatch"
            elif row.get("content_hash") != sha256_value(
                {key: value for key, value in row.items() if key != "content_hash"}
            ):
                reason = "content_hash_mismatch"
            elif link.get("payload_hash") != row.get("content_hash"):
                reason = "payload_hash_mismatch"
            elif link.get("prev") != previous:
                reason = "prev_mismatch"
            elif link.get("hash") != sha256_value(
                {
                    "label": label,
                    "id": identifier,
                    "payload_hash": row.get("content_hash"),
                    "prev": previous,
                }
            ):
                reason = "link_hash_mismatch"
            if reason:
                raise _recovery_error(
                    f"checkpoint chain is broken at link {index}",
                    field=f"snapshot.chain[{index}].hash",
                    index=index,
                    identifier=identifier,
                    reason=reason,
                )
            previous = link["hash"]

    def load_latest(self, *, strict: bool = True) -> RecoveryState:
        try:
            pointer = read_json(self.latest_path)
        except (OSError, ValueError) as exc:
            if strict:
                good = self._find_good_published()
                raise _recovery_error(
                    "latest snapshot pointer is missing or invalid",
                    field="snapshot.latest",
                    reason="latest_pointer_invalid",
                    good_version=good,
                ) from exc
            pointer = None
        if isinstance(pointer, dict):
            snapshot_file = pointer.get("snapshot_file")
            if (
                not isinstance(snapshot_file, str)
                or Path(snapshot_file).name != snapshot_file
            ):
                raise _recovery_error(
                    "latest snapshot pointer lacks snapshot_file",
                    field="snapshot.latest.snapshot_file",
                    reason="missing_field",
                )
            path = self.directory / snapshot_file
            kind = pointer.get("snapshot_kind")
            if kind == "checkpoint":
                return self._load_checkpoint(path, pointer, strict=strict)
            expected_version = pointer.get("latest_version")
            if not isinstance(expected_version, int):
                raise _recovery_error(
                    "latest pointer lacks a valid published version",
                    field="snapshot.latest.latest_version",
                    reason="invalid_version",
                )
            return self._load_published(
                path, strict=strict, expected_version=expected_version
            )

        versions = sorted(
            (
                int(path.stem.split("-v", 1)[1])
                for path in self.directory.glob("snapshot-v*.json")
                if path.stem.split("-v", 1)[1].isdigit()
            ),
            reverse=True,
        )
        if not versions:
            raise _recovery_error(
                "no valid snapshot exists",
                field="snapshot",
                reason="not_found",
            )
        last_error: LCFTError | None = None
        for version in versions:
            try:
                return self._load_published(self._published_path(version), strict=True)
            except LCFTError as exc:
                last_error = exc
        assert last_error is not None
        raise last_error

    def _load_published(
        self,
        path: Path,
        *,
        strict: bool,
        expected_version: int | None = None,
    ) -> RecoveryState:
        try:
            snapshot = read_json(path)
        except (OSError, ValueError) as exc:
            good = self._find_good_published()
            raise _recovery_error(
                "published snapshot cannot be read",
                field="snapshot.file",
                reason="unreadable",
                good_version=good,
            ) from exc
        try:
            version = self._validate_published(snapshot)
        except LCFTError as exc:
            good = self._find_good_published(exclude=snapshot.get("published_version"))
            exc.details = {**exc.details, "recovered_from_good_snapshot_version": good}
            if strict or good is None:
                raise
            return self._load_published(self._published_path(good), strict=True)
        try:
            file_version = int(path.stem.split("-v", 1)[1])
        except (IndexError, ValueError):
            file_version = -1
        if file_version != version:
            raise _recovery_error(
                "snapshot filename version does not match snapshot contents",
                field="snapshot.snapshot_version",
                reason="filename_version_mismatch",
            )
        if expected_version is not None and expected_version != version:
            raise _recovery_error(
                "latest pointer version does not match snapshot contents",
                field="snapshot.latest_version",
                reason="pointer_version_mismatch",
            )
        rows = snapshot["sentences"]
        desc_rows = snapshot["descriptions"]
        return RecoveryState(
            report=deepcopy(snapshot),
            precomputed_sentences=deepcopy(rows),
            precomputed_descriptions=deepcopy(desc_rows),
            completed_ids=[row["id"] for row in rows],
            pending_sentence_ids=[],
            snapshot_version=version,
            recovered_from_published_version=version,
            checkpoint=False,
        )

    def _load_checkpoint(
        self, path: Path, pointer: dict[str, Any], *, strict: bool
    ) -> RecoveryState:
        try:
            checkpoint = read_json(path)
        except (OSError, ValueError) as exc:
            good = self._find_good_published()
            raise _recovery_error(
                "checkpoint cannot be read",
                field="snapshot.file",
                reason="unreadable",
                good_version=good,
            ) from exc
        if not isinstance(checkpoint, dict):
            raise _recovery_error(
                "checkpoint root is not an object", field="snapshot"
            )
        pointer_version = pointer.get("latest_version")
        if (
            not isinstance(pointer_version, int)
            or pointer_version != checkpoint.get("snapshot_version")
        ):
            raise _recovery_error(
                "checkpoint version does not match latest pointer",
                field="snapshot.latest_version",
                reason="pointer_version_mismatch",
            )
        try:
            file_version = int(path.stem.split("-v", 1)[1])
        except (IndexError, ValueError):
            file_version = -1
        if file_version != checkpoint.get("snapshot_version"):
            raise _recovery_error(
                "checkpoint filename version does not match contents",
                field="snapshot.snapshot_version",
                reason="filename_version_mismatch",
            )
        required = (
            "snapshot_version",
            "base_published_version",
            "base_track_hash",
            "params",
            "params_hash",
            "precomputed_sentences",
            "precomputed_descriptions",
            "completed_ids",
            "pending_sentence_ids",
            "chain",
        )
        for key in required:
            if key not in checkpoint:
                raise _recovery_error(
                    f"checkpoint field {key} is missing",
                    field=f"snapshot.{key}",
                    reason="missing_field",
                )
        try:
            self._validate_precompute(checkpoint)
        except LCFTError as exc:
            good = self._find_good_published(
                exclude=checkpoint.get("base_published_version")
            )
            exc.details = {**exc.details, "recovered_from_good_snapshot_version": good}
            if strict or good is None:
                raise
            return self._load_published(self._published_path(good), strict=True)

        base_version = checkpoint.get("base_published_version")
        if not isinstance(base_version, int):
            raise _recovery_error(
                "checkpoint base published version is invalid",
                field="snapshot.base_published_version",
                reason="invalid_version",
            )
        base_path = self._published_path(base_version)
        try:
            base = read_json(base_path)
            self._validate_published(base)
        except (OSError, ValueError, LCFTError) as exc:
            good = self._find_good_published(exclude=base_version)
            raise _recovery_error(
                "checkpoint base snapshot is invalid",
                field="snapshot.base_published_version",
                reason="base_snapshot_invalid",
                good_version=good,
            ) from exc
        if checkpoint.get("base_track_hash") != base.get("track_hash"):
            good = self._find_good_published(exclude=base_version)
            raise _recovery_error(
                "checkpoint base hash does not match published snapshot",
                field="snapshot.base_track_hash",
                reason="base_hash_mismatch",
                good_version=good,
            )
        try:
            params = LayoutParams.from_dict(checkpoint.get("params"))
        except LCFTError as exc:
            raise _recovery_error(
                "checkpoint parameters are invalid",
                field="snapshot.params",
                reason=exc.code,
            ) from exc
        if params.hash != checkpoint.get("params_hash"):
            raise _recovery_error(
                "checkpoint parameter hash is invalid",
                field="snapshot.params_hash",
                reason="params_hash_mismatch",
            )
        completed = [row["id"] for row in checkpoint["precomputed_sentences"]]
        if checkpoint.get("completed_ids") != completed:
            raise _recovery_error(
                "checkpoint completed ids do not match precomputed rows",
                field="snapshot.completed_ids",
                reason="completed_ids_mismatch",
            )
        pending = checkpoint.get("pending_sentence_ids")
        if not isinstance(pending, list) or not all(isinstance(item, str) for item in pending):
            raise _recovery_error(
                "checkpoint pending ids are invalid",
                field="snapshot.pending_sentence_ids",
                reason="pending_ids_invalid",
            )
        return RecoveryState(
            report=deepcopy(base),
            precomputed_sentences=deepcopy(checkpoint["precomputed_sentences"]),
            precomputed_descriptions=deepcopy(
                checkpoint["precomputed_descriptions"]
            ),
            completed_ids=completed,
            pending_sentence_ids=list(pending),
            snapshot_version=int(checkpoint["snapshot_version"]),
            recovered_from_published_version=base_version,
            checkpoint=True,
        )

