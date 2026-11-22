from __future__ import annotations

import json
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .engine import MatchEngine
from .errors import E_DECODE, E_SEQ, E_SEMANTIC, KernelError
from .models import AuthoritativeState, MatchContext
from .util import atomic_write_bytes, canonical_json_bytes, digest_json


_LOCKS_GUARD = threading.Lock()
_PATH_LOCKS: dict[str, threading.RLock] = {}


def _lock_for(path: Path) -> threading.RLock:
    key = str(path.resolve())
    with _LOCKS_GUARD:
        return _PATH_LOCKS.setdefault(key, threading.RLock())


@dataclass
class CheckpointLoadResult:
    valid: bool
    state: AuthoritativeState | None = None
    reason: str = ""
    details: dict[str, Any] = field(default_factory=dict)
    recompute_required: bool = False

    def as_dict(self) -> dict[str, Any]:
        return {
            "valid": self.valid,
            "reason": self.reason,
            "details": self.details,
            "recompute_required": self.recompute_required,
            "restored_tick": self.state.tick if self.state else None,
            "restored_hash": self.state.snapshot_hash if self.state else None,
        }


class CheckpointStore:
    """Atomic, serialized, single-window authority checkpoints."""

    FORMAT_VERSION = 1

    def __init__(
        self,
        path: str | Path,
        *,
        context: MatchContext,
        protocol_version: int,
        commit_window: int,
    ) -> None:
        if commit_window <= 0:
            raise KernelError(
                E_SEMANTIC,
                "commit_window must be positive",
                path="$.commit_window",
            )
        self.path = Path(path)
        self.context = context
        self.protocol_version = protocol_version
        self.commit_window = commit_window
        self._lock = _lock_for(self.path)
        self._last_commit_tick = -1
        self._writes = 0

    def _envelope(
        self,
        state: AuthoritativeState,
        rng_progress: int,
        input_offset: int,
    ) -> dict[str, Any]:
        if state.tick % self.commit_window != 0:
            raise KernelError(
                E_SEMANTIC,
                "checkpoint tick is not aligned to the commit window",
                path="$.tick",
                details={
                    "tick": state.tick,
                    "commit_window": self.commit_window,
                },
            )
        if state.compute_hash() != state.snapshot_hash:
            raise KernelError(
                E_SEMANTIC,
                "state snapshot hash is stale",
                path="$.snapshot_hash",
            )
        payload = {
            "format": "authority-window-checkpoint",
            "format_version": self.FORMAT_VERSION,
            "protocol_version": self.protocol_version,
            "match_id": self.context.match_id,
            "match_seed": self.context.seed,
            "input_fingerprint": self.context.input_fingerprint,
            "commit_window": self.commit_window,
            "commit_tick": state.tick,
            "window_index": state.tick // self.commit_window,
            "state": state.as_checkpoint_dict(),
            "snapshot_content_hash": state.snapshot_hash,
            "rng_progress": rng_progress,
            "input_consumption_offset": input_offset,
        }
        payload["envelope_hash"] = digest_json(payload)
        return payload

    def save(
        self,
        state: AuthoritativeState,
        *,
        rng_progress: int | None = None,
        input_offset: int | None = None,
    ) -> dict[str, Any]:
        rng_value = state.rng_progress if rng_progress is None else rng_progress
        offset_value = state.input_offset if input_offset is None else input_offset
        with self._lock:
            if state.tick <= self._last_commit_tick:
                raise KernelError(
                    E_SEQ,
                    "checkpoint commit order must be strictly increasing",
                    path="$.commit_tick",
                    details={
                        "last_commit_tick": self._last_commit_tick,
                        "incoming_tick": state.tick,
                    },
                )
            payload = self._envelope(state, rng_value, offset_value)
            data = canonical_json_bytes(payload) + b"\n"
            atomic_write_bytes(self.path, data)
            self._last_commit_tick = state.tick
            self._writes += 1
            return {
                "path": str(self.path),
                "commit_tick": state.tick,
                "bytes": len(data),
                "snapshot_content_hash": state.snapshot_hash,
                "envelope_hash": payload["envelope_hash"],
            }

    def _invalidate(self, reason: str, details: dict[str, Any]) -> None:
        marker = {
            "invalidated": True,
            "reason": reason,
            "details": details,
            "path": str(self.path),
        }
        atomic_write_bytes(
            self.path.with_name(self.path.name + ".invalidated.json"),
            canonical_json_bytes(marker) + b"\n",
        )

    def load(self) -> CheckpointLoadResult:
        with self._lock:
            if not self.path.exists():
                return CheckpointLoadResult(
                    valid=False,
                    reason="CHECKPOINT_NOT_FOUND",
                    recompute_required=True,
                )
            try:
                document = json.loads(self.path.read_text(encoding="utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
                details = {"error": str(exc)}
                self._invalidate("CHECKPOINT_CORRUPT", details)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_DECODE,
                    details=details,
                    recompute_required=True,
                )
            if not isinstance(document, dict):
                details = {"error": "checkpoint root is not an object"}
                self._invalidate("CHECKPOINT_CORRUPT", details)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_DECODE,
                    details=details,
                    recompute_required=True,
                )
            declared_hash = document.get("envelope_hash")
            checked = {key: value for key, value in document.items() if key != "envelope_hash"}
            if declared_hash != digest_json(checked):
                details = {
                    "declared": declared_hash,
                    "computed": digest_json(checked),
                }
                self._invalidate("CHECKPOINT_HASH_MISMATCH", details)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_DECODE,
                    details=details,
                    recompute_required=True,
                )
            expected = {
                "protocol_version": self.protocol_version,
                "match_id": self.context.match_id,
                "match_seed": self.context.seed,
                "input_fingerprint": self.context.input_fingerprint,
                "commit_window": self.commit_window,
            }
            mismatches = {
                key: {"expected": value, "actual": document.get(key)}
                for key, value in expected.items()
                if document.get(key) != value
            }
            if mismatches:
                self._invalidate("CHECKPOINT_IDENTITY_MISMATCH", mismatches)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_SEMANTIC,
                    details={"mismatches": mismatches},
                    recompute_required=True,
                )
            try:
                state = AuthoritativeState.from_checkpoint_dict(document["state"])
                state.validate()
            except (KeyError, TypeError, ValueError, KernelError) as exc:
                details = {"error": str(exc)}
                self._invalidate("CHECKPOINT_STATE_INVALID", details)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_DECODE,
                    details=details,
                    recompute_required=True,
                )
            if (
                state.compute_hash() != state.snapshot_hash
                or state.snapshot_hash != document.get("snapshot_content_hash")
            ):
                details = {
                    "state_hash": state.snapshot_hash,
                    "computed_hash": state.compute_hash(),
                    "declared_content_hash": document.get("snapshot_content_hash"),
                }
                self._invalidate("CHECKPOINT_SNAPSHOT_HASH_MISMATCH", details)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_DECODE,
                    details=details,
                    recompute_required=True,
                )
            if (
                document.get("rng_progress") != state.rng_progress
                or document.get("input_consumption_offset") != state.input_offset
            ):
                details = {
                    "declared_rng_progress": document.get("rng_progress"),
                    "state_rng_progress": state.rng_progress,
                    "declared_input_offset": document.get("input_consumption_offset"),
                    "state_input_offset": state.input_offset,
                }
                self._invalidate("CHECKPOINT_PROGRESS_MISMATCH", details)
                return CheckpointLoadResult(
                    valid=False,
                    reason=E_SEMANTIC,
                    details=details,
                    recompute_required=True,
                )
            self._last_commit_tick = state.tick
            return CheckpointLoadResult(valid=True, state=state)


@dataclass
class RecoveryResult:
    checkpoint_valid: bool
    checkpoint_reason: str
    resumed_tick: int
    recomputed_from_tick: int
    final_state: AuthoritativeState
    complete: bool
    cancelled: bool
    forced_termination_detected: bool
    restored_only: bool
    states: list[AuthoritativeState] = field(default_factory=list)

    def as_dict(self) -> dict[str, Any]:
        return {
            "checkpoint_valid": self.checkpoint_valid,
            "checkpoint_reason": self.checkpoint_reason,
            "resumed_tick": self.resumed_tick,
            "recomputed_from_tick": self.recomputed_from_tick,
            "final_tick": self.final_state.tick,
            "final_hash": self.final_state.snapshot_hash,
            "complete": self.complete,
            "cancelled": self.cancelled,
            "forced_termination_detected": self.forced_termination_detected,
            "restored_only": self.restored_only,
            "recomputed_tick_count": len(self.states),
        }


def resume_match(
    *,
    engine: MatchEngine,
    store: CheckpointStore,
    target_tick: int,
    save_commits: bool = True,
    force_terminate_at: int | None = None,
    cancel_event: threading.Event | None = None,
) -> RecoveryResult:
    loaded = store.load()
    if loaded.valid and loaded.state is not None:
        start = loaded.state.clone()
        checkpoint_valid = True
        reason = ""
        resumed_tick = start.tick
        recomputed_from_tick = start.tick
    else:
        start = engine.initial_state.clone()
        checkpoint_valid = False
        reason = loaded.reason
        resumed_tick = 0
        recomputed_from_tick = 0
    if start.tick > target_tick:
        raise KernelError(
            E_SEMANTIC,
            "checkpoint is newer than the requested target tick",
            path="$.target_tick",
            details={"checkpoint_tick": start.tick, "target_tick": target_tick},
        )
    state = start
    states = [state.clone()]
    if (
        save_commits
        and not checkpoint_valid
        and state.tick % store.commit_window == 0
    ):
        store.save(state)
    stop_tick = target_tick if force_terminate_at is None else min(
        target_tick, force_terminate_at
    )
    cancelled = False
    while state.tick < stop_tick:
        if cancel_event is not None and cancel_event.is_set():
            cancelled = True
            break
        state = engine.advance(state)
        states.append(state.clone())
        if save_commits and state.tick % store.commit_window == 0:
            store.save(state)
    complete = state.tick >= target_tick and not cancelled
    return RecoveryResult(
        checkpoint_valid=checkpoint_valid,
        checkpoint_reason=reason,
        resumed_tick=resumed_tick,
        recomputed_from_tick=recomputed_from_tick,
        final_state=state,
        complete=complete,
        cancelled=cancelled,
        forced_termination_detected=(
            force_terminate_at is not None and state.tick < target_tick
        ),
        restored_only=(checkpoint_valid and state.tick < target_tick),
        states=states,
    )

