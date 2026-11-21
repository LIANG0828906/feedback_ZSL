"""Atomic per-step snapshots and hash-chain recovery."""

from __future__ import annotations

import json
import os
import re
import tempfile
from dataclasses import dataclass
from typing import Any

from .errors import E_RECOVERY_INVALID, fail
from .model import (
    SNAPSHOT_VERSION,
    STATE_MODEL_VERSION,
    ZERO_HASH,
    RoomState,
    EntityState,
    canonical_json,
    chain_hash as compute_chain_hash,
    state_from_dict,
    state_hash,
    state_to_dict,
)


SNAPSHOT_NAME = re.compile(r"^step-(\d{6,})\.json$")
REQUIRED_SNAPSHOT_FIELDS = (
    "snapshot_version",
    "match_id",
    "mode",
    "step",
    "state_model_version",
    "config_digest",
    "state",
    "hash_chain_prefix",
    "state_hash",
    "chain_hash",
    "error_summary",
    "error_state",
    "effective_ops",
)


@dataclass
class Snapshot:
    path: str
    step: int
    mode: str
    state_hash: str
    chain_hash: str
    chain_prefix: str
    room: RoomState
    entities: dict[str, EntityState]
    error_summary: dict[str, Any]
    error_state: dict[str, Any]
    effective_ops: list[dict[str, Any]]


class SnapshotStore:
    def __init__(
        self,
        directory: str,
        *,
        match_id: str,
        mode: str,
        config_digest: str,
    ) -> None:
        self.directory = os.path.abspath(directory)
        self.match_id = match_id
        self.mode = mode
        self.config_digest = config_digest
        os.makedirs(self.directory, exist_ok=True)

    def step_path(self, step: int) -> str:
        return os.path.join(self.directory, f"step-{step:06d}.json")

    def commit(
        self,
        *,
        step: int,
        room: RoomState,
        entities: dict[str, EntityState],
        state_digest: str,
        chain_prefix: str,
        chain_digest: str,
        error_summary: dict[str, Any],
        error_state: dict[str, Any],
        effective_ops: list[dict[str, Any]],
    ) -> str:
        state = state_to_dict(
            step,
            room,
            entities,
            numeric=self.mode,
            version=STATE_MODEL_VERSION,
        )
        payload = {
            "snapshot_version": SNAPSHOT_VERSION,
            "match_id": self.match_id,
            "mode": self.mode,
            "step": step,
            "state_model_version": STATE_MODEL_VERSION,
            "config_digest": self.config_digest,
            "state": state,
            "hash_chain_prefix": chain_prefix,
            "state_hash": state_digest,
            "chain_hash": chain_digest,
            "error_summary": error_summary,
            "error_state": error_state,
            "effective_ops": effective_ops,
        }
        target = self.step_path(step)
        fd, temp_path = tempfile.mkstemp(
            prefix=f".tmp-step-{step:06d}-",
            dir=self.directory,
        )
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                handle.write(canonical_json(payload))
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_path, target)
            self._fsync_directory()
        except Exception:
            try:
                os.unlink(temp_path)
            except OSError:
                pass
            raise
        return target

    def _fsync_directory(self) -> None:
        try:
            fd = os.open(self.directory, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    def snapshot_files(self) -> list[tuple[int, str]]:
        result: list[tuple[int, str]] = []
        for name in os.listdir(self.directory):
            match = SNAPSHOT_NAME.match(name)
            if not match:
                continue
            result.append((int(match.group(1)), os.path.join(self.directory, name)))
        result.sort()
        return result

    def load_latest(self) -> Snapshot | None:
        files = self.snapshot_files()
        if not files:
            return None
        expected = 1
        previous_chain = ZERO_HASH
        latest: Snapshot | None = None
        for step, path in files:
            if step != expected:
                fail(
                    E_RECOVERY_INVALID,
                    f"snapshot chain gap: expected step {expected}, found {step}",
                    "snapshot.step",
                    step=expected,
                    details={"expected": expected, "found": step, "path": path},
                )
            latest = self._load_one(
                path,
                expected_step=step,
                previous_chain=previous_chain,
            )
            previous_chain = latest.chain_hash
            expected += 1
        return latest

    def _load_one(
        self,
        path: str,
        *,
        expected_step: int,
        previous_chain: str,
    ) -> Snapshot:
        try:
            with open(path, "r", encoding="utf-8") as handle:
                payload = json.load(handle)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            fail(
                E_RECOVERY_INVALID,
                f"snapshot is truncated or invalid JSON: {exc}",
                path,
                step=expected_step,
                details={"path": path},
            )
        except OSError as exc:
            fail(
                E_RECOVERY_INVALID,
                f"cannot read snapshot: {exc}",
                path,
                step=expected_step,
            )
        if not isinstance(payload, dict):
            fail(
                E_RECOVERY_INVALID,
                "snapshot must be a JSON object",
                "snapshot",
                step=expected_step,
            )
        for field_name in REQUIRED_SNAPSHOT_FIELDS:
            if field_name not in payload:
                fail(
                    E_RECOVERY_INVALID,
                    f"snapshot is missing {field_name}",
                    f"snapshot.{field_name}",
                    step=expected_step,
                )
        if payload["snapshot_version"] != SNAPSHOT_VERSION:
            fail(
                E_RECOVERY_INVALID,
                "unsupported snapshot version",
                "snapshot.snapshot_version",
                step=expected_step,
                details={"expected": SNAPSHOT_VERSION, "found": payload["snapshot_version"]},
            )
        if payload["match_id"] != self.match_id:
            fail(
                E_RECOVERY_INVALID,
                "snapshot belongs to a different match",
                "snapshot.match_id",
                step=expected_step,
            )
        if payload["mode"] != self.mode:
            fail(
                E_RECOVERY_INVALID,
                "snapshot numeric mode does not match recovery mode",
                "snapshot.mode",
                step=expected_step,
            )
        if payload["config_digest"] != self.config_digest:
            fail(
                E_RECOVERY_INVALID,
                "snapshot configuration digest does not match recording",
                "snapshot.config_digest",
                step=expected_step,
            )
        try:
            actual_step = int(payload["step"])
        except (TypeError, ValueError):
            fail(
                E_RECOVERY_INVALID,
                "snapshot step is not an integer",
                "snapshot.step",
                step=expected_step,
            )
        if actual_step != expected_step:
            fail(
                E_RECOVERY_INVALID,
                "snapshot step does not match file name",
                "snapshot.step",
                step=expected_step,
            )
        if payload["hash_chain_prefix"] != previous_chain:
            fail(
                E_RECOVERY_INVALID,
                "hash chain prefix does not match previous snapshot",
                "snapshot.hash_chain_prefix",
                step=expected_step,
            )
        model_version = payload["state_model_version"]
        if model_version != STATE_MODEL_VERSION:
            fail(
                E_RECOVERY_INVALID,
                "snapshot state model version mismatch",
                "snapshot.state_model_version",
                step=expected_step,
            )
        try:
            state_step, room, entities = state_from_dict(
                payload["state"],
                numeric=self.mode,
                expected_version=STATE_MODEL_VERSION,
            )
        except Exception as exc:
            if hasattr(exc, "code"):
                raise
            fail(
                E_RECOVERY_INVALID,
                f"snapshot state is invalid: {exc}",
                "snapshot.state",
                step=expected_step,
            )
        if state_step != expected_step:
            fail(
                E_RECOVERY_INVALID,
                "snapshot state step mismatch",
                "snapshot.state.step",
                step=expected_step,
            )
        recomputed_state_hash = state_hash(
            state_step,
            room,
            entities,
            numeric=self.mode,
            version=STATE_MODEL_VERSION,
        )
        if recomputed_state_hash != payload["state_hash"]:
            fail(
                E_RECOVERY_INVALID,
                "snapshot state hash mismatch",
                "snapshot.state_hash",
                step=expected_step,
                details={
                    "expected": payload["state_hash"],
                    "actual": recomputed_state_hash,
                },
            )
        recomputed_chain = compute_chain_hash(previous_chain, recomputed_state_hash)
        if recomputed_chain != payload["chain_hash"]:
            fail(
                E_RECOVERY_INVALID,
                "snapshot hash chain mismatch",
                "snapshot.chain_hash",
                step=expected_step,
                details={"expected": payload["chain_hash"], "actual": recomputed_chain},
            )
        if not isinstance(payload["error_summary"], dict):
            fail(
                E_RECOVERY_INVALID,
                "snapshot error_summary must be an object",
                "snapshot.error_summary",
                step=expected_step,
            )
        if not isinstance(payload["error_state"], dict):
            fail(
                E_RECOVERY_INVALID,
                "snapshot error_state must be an object",
                "snapshot.error_state",
                step=expected_step,
            )
        if not isinstance(payload["effective_ops"], list):
            fail(
                E_RECOVERY_INVALID,
                "snapshot effective_ops must be an array",
                "snapshot.effective_ops",
                step=expected_step,
            )
        return Snapshot(
            path=path,
            step=actual_step,
            mode=self.mode,
            state_hash=payload["state_hash"],
            chain_hash=payload["chain_hash"],
            chain_prefix=previous_chain,
            room=room,
            entities=entities,
            error_summary=payload["error_summary"],
            error_state=payload["error_state"],
            effective_ops=payload["effective_ops"],
        )
