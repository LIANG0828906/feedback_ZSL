"""Atomic per-step snapshots protected by a forward hash chain."""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .config import ReplayConfig
from .errors import BusinessError, RecoveryInvalidError
from .util import atomic_write_json, canonical_json, load_json, sha256_bytes

SNAPSHOT_NAME = re.compile(r"^step-(\d{6,})\.json$")
GENESIS_HASH = sha256_bytes(b"cover-replay-snapshot-genesis-v1")


def _recovery(step: int, message: str, **details: Any) -> RecoveryInvalidError:
    return RecoveryInvalidError(step, message, **details)


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _validate_nested_snapshot(payload: dict[str, Any], step: int) -> None:
    if not isinstance(payload.get("meta"), dict):
        raise _recovery(step, f"snapshot step {step} has no meta object")
    if not isinstance(payload.get("input_digest"), str):
        raise _recovery(step, f"snapshot step {step} has no input digest")
    if not isinstance(payload.get("chain_hash"), str):
        raise _recovery(step, f"snapshot step {step} has no chain hash")
    state = payload["state"]
    if not _is_int(state.get("step")) or state["step"] != step:
        raise _recovery(step, f"snapshot step {step} has inconsistent state step")
    frame = payload["frame"]
    frame_required = {
        "step",
        "state_hash",
        "covers",
        "credits",
        "credit_entries",
        "crack_events",
        "effective_client_sequences",
        "budget_usage",
        "scheduling",
    }
    missing_frame = sorted(frame_required - set(frame))
    if missing_frame:
        raise _recovery(
            step,
            f"snapshot step {step} frame is missing fields",
            missing_fields=missing_frame,
        )
    if not _is_int(frame["step"]) or frame["step"] != step:
        raise _recovery(step, f"snapshot step {step} has inconsistent frame step")
    if not isinstance(frame["state_hash"], str):
        raise _recovery(step, f"snapshot step {step} frame has no state hash")
    if not isinstance(frame["covers"], list):
        raise _recovery(step, f"snapshot step {step} frame covers is not an array")
    if not isinstance(frame["credits"], list):
        raise _recovery(step, f"snapshot step {step} frame credits is not an array")
    if not isinstance(frame["effective_client_sequences"], list):
        raise _recovery(
            step,
            f"snapshot step {step} effective client sequences is not an array",
        )
    if not isinstance(frame["budget_usage"], dict):
        raise _recovery(step, f"snapshot step {step} has no budget usage")
    budget_required = {
        "memory_budget",
        "peak_memory_used",
        "queue_watermark",
        "worker_limit",
        "wave_count",
    }
    missing_budget = sorted(budget_required - set(frame["budget_usage"]))
    if missing_budget:
        raise _recovery(
            step,
            f"snapshot step {step} budget usage is missing fields",
            missing_fields=missing_budget,
        )
    if not all(
        _is_int(frame["budget_usage"][name]) for name in budget_required
    ):
        raise _recovery(step, f"snapshot step {step} budget usage is invalid")
    if not isinstance(frame["scheduling"], dict):
        raise _recovery(step, f"snapshot step {step} has no scheduling trace")
    if not isinstance(frame["scheduling"].get("trace"), list):
        raise _recovery(step, f"snapshot step {step} scheduling trace is invalid")
    for index, raw_cover in enumerate(frame["covers"]):
        if not isinstance(raw_cover, dict) or not {
            "block_id",
            "accumulated_damage",
            "remaining",
            "cracked",
            "cracked_step",
        } <= set(raw_cover):
            raise _recovery(
                step,
                f"snapshot step {step} frame cover {index} is invalid",
            )
    for index, credit in enumerate(frame["credits"]):
        if not isinstance(credit, dict) or not {
            "block_id",
            "predicted_remaining",
        } <= set(credit):
            raise _recovery(
                step,
                f"snapshot step {step} frame credit {index} is invalid",
            )
    cover_required = {
        "block_id",
        "durability_max",
        "accumulated_damage",
        "cracked",
        "cracked_step",
    }
    seen_blocks: set[int] = set()
    for index, raw_cover in enumerate(state["covers"]):
        if not isinstance(raw_cover, dict):
            raise _recovery(
                step, f"snapshot step {step} cover state {index} is not an object"
            )
        missing = sorted(cover_required - set(raw_cover))
        if missing:
            raise _recovery(
                step,
                f"snapshot step {step} cover state {index} is missing fields",
                missing_fields=missing,
            )
        if not all(
            _is_int(raw_cover[name])
            for name in ("block_id", "durability_max", "accumulated_damage")
        ):
            raise _recovery(
                step, f"snapshot step {step} cover state {index} has invalid integers"
            )
        if not isinstance(raw_cover["cracked"], bool):
            raise _recovery(
                step, f"snapshot step {step} cover state {index} has invalid cracked flag"
            )
        if raw_cover["cracked_step"] is not None and not _is_int(
            raw_cover["cracked_step"]
        ):
            raise _recovery(
                step, f"snapshot step {step} cover state {index} has invalid cracked step"
            )
        if (
            raw_cover["durability_max"] < 0
            or raw_cover["accumulated_damage"] < 0
            or raw_cover["accumulated_damage"] > raw_cover["durability_max"]
        ):
            raise _recovery(
                step, f"snapshot step {step} cover state {index} is out of range"
            )
        if raw_cover["block_id"] in seen_blocks:
            raise _recovery(
                step, f"snapshot step {step} has duplicate cover states"
            )
        seen_blocks.add(raw_cover["block_id"])
    ledger = payload["ledger"]
    for block_id, value in ledger.items():
        try:
            numeric_block_id = int(block_id)
        except (TypeError, ValueError):
            raise _recovery(
                step, f"snapshot step {step} ledger has an invalid block key"
            ) from None
        if str(numeric_block_id) != str(block_id):
            raise _recovery(
                step, f"snapshot step {step} ledger has an invalid block key"
            )
        if not _is_int(value) or value < 0:
            raise _recovery(
                step,
                f"snapshot step {step} ledger value for block {block_id} is invalid",
            )


@dataclass(frozen=True, slots=True)
class LoadedSnapshot:
    step: int
    chain_hash: str
    record: dict[str, Any]
    snapshot_count: int


class SnapshotStore:
    def __init__(
        self,
        directory: str | Path | None,
        *,
        input_digest: str,
        config: ReplayConfig,
    ) -> None:
        self.directory = Path(directory) if directory is not None else None
        self.input_digest = input_digest
        self.config = config
        self.last_hash = GENESIS_HASH
        self.last_step = 0
        if self.directory is not None:
            self.directory.mkdir(parents=True, exist_ok=True)

    def ensure_fresh_target(self) -> None:
        if self.directory is None or self.last_step:
            return
        existing = [
            path
            for path in self.directory.iterdir()
            if SNAPSHOT_NAME.match(path.name)
        ]
        if existing:
            raise BusinessError(
                "E_SNAPSHOT_EXISTS",
                "snapshot directory already contains committed steps",
                directory=str(self.directory),
                suggestion="resume from the snapshots or clear them for a fresh run",
                first_step=min(
                    int(SNAPSHOT_NAME.match(path.name).group(1))
                    for path in existing
                    if SNAPSHOT_NAME.match(path.name)
                ),
            )

    def path_for_step(self, step: int) -> Path:
        if self.directory is None:
            raise RuntimeError("snapshot storage is disabled")
        return self.directory / f"step-{step:06d}.json"

    def write_step(
        self,
        *,
        step: int,
        engine_state: dict[str, Any],
        ledger: dict[str, int],
        frame: dict[str, Any],
        meta: dict[str, Any],
    ) -> str | None:
        if self.directory is None:
            return None
        if step != self.last_step + 1:
            raise ValueError(
                f"snapshot step must be contiguous: expected {self.last_step + 1}, got {step}"
            )
        body: dict[str, Any] = {
            "snapshot_version": self.config.snapshot_version,
            "step": step,
            "previous_hash": self.last_hash,
            "input_digest": self.input_digest,
            "meta": meta,
            "state": engine_state,
            "ledger": ledger,
            "frame": frame,
        }
        chain_hash = sha256_bytes(
            (self.last_hash + "\n" + canonical_json(body)).encode("utf-8")
        )
        record = dict(body)
        record["chain_hash"] = chain_hash
        atomic_write_json(self.path_for_step(step), record)
        self.last_hash = chain_hash
        self.last_step = step
        return chain_hash

    def load_latest(self) -> LoadedSnapshot | None:
        if self.directory is None or not self.directory.exists():
            return None
        numbered: list[tuple[int, Path]] = []
        for path in self.directory.iterdir():
            match = SNAPSHOT_NAME.match(path.name)
            if match is None:
                continue
            numbered.append((int(match.group(1)), path))
        numbered.sort()
        if not numbered:
            return None
        expected_step = 1
        previous_hash = GENESIS_HASH
        latest: LoadedSnapshot | None = None
        for step, path in numbered:
            if step != expected_step:
                raise RecoveryInvalidError(
                    expected_step,
                    f"missing committed snapshot for step {expected_step}",
                    found_step=step,
                )
            try:
                payload = load_json(path)
            except (OSError, ValueError, json.JSONDecodeError) as error:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot step {step} is not readable JSON: {error}",
                ) from None
            if not isinstance(payload, dict):
                raise RecoveryInvalidError(
                    step, f"snapshot step {step} root is not an object"
                )
            required = {
                "snapshot_version",
                "step",
                "previous_hash",
                "input_digest",
                "meta",
                "state",
                "ledger",
                "frame",
                "chain_hash",
            }
            missing = sorted(required - set(payload))
            if missing:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot step {step} is missing fields: {', '.join(missing)}",
                    missing_fields=missing,
                )
            if payload["snapshot_version"] != self.config.snapshot_version:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot version mismatch at step {step}",
                    snapshot_version=payload["snapshot_version"],
                    expected_version=self.config.snapshot_version,
                )
            if payload["step"] != step:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot filename step {step} disagrees with payload step {payload['step']}",
                )
            if payload["input_digest"] != self.input_digest:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot input digest mismatch at step {step}",
                )
            if payload["previous_hash"] != previous_hash:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot hash-chain break at step {step}",
                )
            body = {key: value for key, value in payload.items() if key != "chain_hash"}
            calculated = sha256_bytes(
                (previous_hash + "\n" + canonical_json(body)).encode("utf-8")
            )
            if calculated != payload["chain_hash"]:
                raise RecoveryInvalidError(
                    step,
                    f"snapshot payload hash mismatch at step {step}",
                )
            state = payload["state"]
            if not isinstance(state, dict) or not isinstance(
                state.get("covers"), list
            ):
                raise RecoveryInvalidError(
                    step, f"snapshot step {step} has no cover state"
                )
            if not isinstance(payload["ledger"], dict):
                raise RecoveryInvalidError(
                    step, f"snapshot step {step} has no credit ledger"
                )
            if not isinstance(payload["frame"], dict):
                raise RecoveryInvalidError(
                    step, f"snapshot step {step} has no committed frame"
                )
            _validate_nested_snapshot(payload, step)
            previous_hash = payload["chain_hash"]
            latest = LoadedSnapshot(
                step=step,
                chain_hash=payload["chain_hash"],
                record=payload,
                snapshot_count=expected_step,
            )
            expected_step += 1
        self.last_hash = previous_hash
        self.last_step = latest.step if latest else 0
        return latest
