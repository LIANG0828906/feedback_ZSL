"""Atomic per-step snapshots and hash-chain validation."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any

from .errors import E_IO, E_RECOVERY_INVALID, ReplayError
from .state import ReplayState

SNAPSHOT_VERSION = 1
GENESIS_CHAIN_HASH = hashlib.sha256(b"cover-replay-genesis-v1").hexdigest()


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def compute_state_hash(scenario_fingerprint: str, state_payload: dict[str, Any]) -> str:
    return hashlib.sha256(
        canonical_json_bytes(
            {
                "scenario_fingerprint": scenario_fingerprint,
                "state": state_payload,
            }
        )
    ).hexdigest()


def compute_chain_hash(previous_chain_hash: str, state_hash: str) -> str:
    return hashlib.sha256(
        f"{previous_chain_hash}:{state_hash}".encode()
    ).hexdigest()


def make_snapshot_document(
    scenario_fingerprint: str,
    state: ReplayState,
    frame: dict[str, Any],
    previous_chain_hash: str,
) -> dict[str, Any]:
    state_payload = state.to_payload()
    state_hash = compute_state_hash(scenario_fingerprint, state_payload)
    chain_hash = compute_chain_hash(previous_chain_hash, state_hash)
    state.state_hash = state_hash
    state.chain_hash = chain_hash
    return {
        "snapshot_version": SNAPSHOT_VERSION,
        "scenario_fingerprint": scenario_fingerprint,
        "step": state.step,
        "state": state_payload,
        "state_hash": state_hash,
        "prev_chain_hash": previous_chain_hash,
        "chain_hash": chain_hash,
        "hash_chain_prefix": {
            "length": state.step + 1,
            "digest": chain_hash,
        },
        "frame": frame,
    }


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=path.parent,
    )
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
        _fsync_directory(path.parent)
    except Exception as error:
        try:
            temporary_path.unlink(missing_ok=True)
        except OSError:
            pass
        raise ReplayError(
            E_IO,
            f"failed to atomically write {path.name}: {error}",
            None,
            str(path),
        ) from error


def write_snapshot(snapshot_dir: Path, document: dict[str, Any]) -> Path:
    path = snapshot_dir / f"step_{document['step']:06d}.json"
    atomic_write_bytes(path, canonical_json_bytes(document) + b"\n")
    return path


def snapshot_paths(snapshot_dir: Path) -> list[Path]:
    if not snapshot_dir.is_dir():
        return []

    def sort_key(path: Path) -> tuple[int, str]:
        stem = path.stem
        if stem.startswith("step_"):
            try:
                return int(stem[5:]), path.name
            except ValueError:
                pass
        return 10**12, path.name

    return sorted(
        (path for path in snapshot_dir.glob("step_*.json") if path.is_file()),
        key=sort_key,
    )


def _snapshot_error(step: int | None, field_path: str, message: str, details: dict[str, Any] | None = None) -> ReplayError:
    payload = {"first_break_step": step}
    if details:
        payload.update(details)
    return ReplayError(
        E_RECOVERY_INVALID,
        message,
        step,
        field_path,
        payload,
    )


def _validate_state_payload_shape(
    payload: dict[str, Any],
    step: int,
    path: str,
) -> None:
    required = (
        "step",
        "covers",
        "ledger",
        "compensations",
        "revocation_log",
        "pending_revokes",
        "settled_shot_keys",
        "buffer_watermark_bytes",
        "last_schedule",
        "last_crack_events",
        "last_predicted_ledger",
    )
    for key in required:
        if key not in payload:
            raise _snapshot_error(
                step,
                f"{path}.{key}",
                "required snapshot state field is missing",
            )
    if payload["step"] != step:
        raise _snapshot_error(
            step,
            f"{path}.step",
            "state payload step does not match snapshot step",
            {"expected": step, "actual": payload["step"]},
        )
    cover_required = (
        "cover_id",
        "damage_units",
        "direct_damage_units",
        "propagated_damage_units",
        "predicted_damage_units",
        "crack_segments",
        "quantization_operations",
    )
    if not isinstance(payload["covers"], list):
        raise _snapshot_error(step, f"{path}.covers", "covers must be an array")
    for index, raw_cover in enumerate(payload["covers"]):
        if not isinstance(raw_cover, dict):
            raise _snapshot_error(
                step,
                f"{path}.covers[{index}]",
                "cover state must be an object",
            )
        for key in cover_required:
            if key not in raw_cover:
                raise _snapshot_error(
                    step,
                    f"{path}.covers[{index}].{key}",
                    "required cover state field is missing",
                )
    ledger_required = (
        "shot_key",
        "player_id",
        "client_seq",
        "cover_id",
        "effective_step",
        "arrival_seq",
        "predicted_units",
        "source_index",
        "status",
        "settled_step",
        "authoritative_units",
        "predicted_after_units",
        "authoritative_after_units",
    )
    if not isinstance(payload["ledger"], list):
        raise _snapshot_error(step, f"{path}.ledger", "ledger must be an array")
    for index, raw_entry in enumerate(payload["ledger"]):
        if not isinstance(raw_entry, dict):
            raise _snapshot_error(
                step,
                f"{path}.ledger[{index}]",
                "ledger entry must be an object",
            )
        for key in ledger_required:
            if key not in raw_entry:
                raise _snapshot_error(
                    step,
                    f"{path}.ledger[{index}].{key}",
                    "required ledger field is missing",
                )
    for key in (
        "compensations",
        "revocation_log",
        "pending_revokes",
        "settled_shot_keys",
        "last_crack_events",
        "last_predicted_ledger",
    ):
        if not isinstance(payload[key], list):
            raise _snapshot_error(step, f"{path}.{key}", f"{key} must be an array")
    if not isinstance(payload["last_schedule"], dict):
        raise _snapshot_error(
            step,
            f"{path}.last_schedule",
            "last_schedule must be an object",
        )
    watermark = payload["buffer_watermark_bytes"]
    if isinstance(watermark, bool) or not isinstance(watermark, int) or watermark < 0:
        raise _snapshot_error(
            step,
            f"{path}.buffer_watermark_bytes",
            "buffer watermark must be a non-negative integer",
        )


def load_and_validate_snapshots(
    snapshot_dir: Path,
    scenario_fingerprint: str,
) -> tuple[list[dict[str, Any]], ReplayState, str]:
    paths = snapshot_paths(snapshot_dir)
    if not paths:
        return [], ReplayState(step=-1, covers={}), GENESIS_CHAIN_HASH
    previous_chain = GENESIS_CHAIN_HASH
    documents: list[dict[str, Any]] = []
    for expected_step, path in enumerate(paths):
        try:
            raw = path.read_bytes()
        except OSError as error:
            raise _snapshot_error(expected_step, str(path), f"cannot read snapshot: {error}") from error
        try:
            document = json.loads(raw)
        except json.JSONDecodeError as error:
            raise _snapshot_error(
                expected_step,
                str(path),
                "snapshot is not valid JSON",
                {"line": error.lineno, "column": error.colno},
            ) from None
        if not isinstance(document, dict):
            raise _snapshot_error(expected_step, str(path), "snapshot root must be an object")
        required = (
            "snapshot_version",
            "scenario_fingerprint",
            "step",
            "state",
            "state_hash",
            "prev_chain_hash",
            "chain_hash",
            "hash_chain_prefix",
            "frame",
        )
        for key in required:
            if key not in document:
                raise _snapshot_error(
                    expected_step,
                    f"{path.name}.{key}",
                    "required snapshot field is missing",
                )
        if document["snapshot_version"] != SNAPSHOT_VERSION:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.snapshot_version",
                "unsupported snapshot version",
                {
                    "expected": SNAPSHOT_VERSION,
                    "actual": document["snapshot_version"],
                },
            )
        step = document["step"]
        if not isinstance(step, int) or step < 0:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.step",
                "snapshot step must be a non-negative integer",
            )
        if step != expected_step:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.step",
                "snapshot sequence has a gap or duplicate",
                {"actual_step": step, "expected_step": expected_step},
            )
        if document["scenario_fingerprint"] != scenario_fingerprint:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.scenario_fingerprint",
                "snapshot belongs to a different scenario",
                {
                    "expected": scenario_fingerprint,
                    "actual": document["scenario_fingerprint"],
                },
            )
        state_payload = document["state"]
        if not isinstance(state_payload, dict):
            raise _snapshot_error(
                expected_step,
                f"{path.name}.state",
                "snapshot state must be an object",
            )
        _validate_state_payload_shape(
            state_payload,
            expected_step,
            f"{path.name}.state",
        )
        try:
            recomputed_state_hash = compute_state_hash(scenario_fingerprint, state_payload)
        except (TypeError, ValueError, KeyError) as error:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.state",
                f"snapshot state cannot be hashed: {error}",
            ) from error
        if document["state_hash"] != recomputed_state_hash:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.state_hash",
                "state hash mismatch",
                {
                    "expected": recomputed_state_hash,
                    "actual": document["state_hash"],
                },
            )
        if document["prev_chain_hash"] != previous_chain:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.prev_chain_hash",
                "hash chain predecessor mismatch",
                {
                    "expected": previous_chain,
                    "actual": document["prev_chain_hash"],
                },
            )
        recomputed_chain_hash = compute_chain_hash(previous_chain, recomputed_state_hash)
        if document["chain_hash"] != recomputed_chain_hash:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.chain_hash",
                "hash chain digest mismatch",
                {
                    "expected": recomputed_chain_hash,
                    "actual": document["chain_hash"],
                },
            )
        prefix = document["hash_chain_prefix"]
        if not isinstance(prefix, dict) or prefix.get("length") != step + 1:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.hash_chain_prefix.length",
                "hash chain prefix summary is invalid",
                {
                    "expected": step + 1,
                    "actual": prefix.get("length") if isinstance(prefix, dict) else None,
                },
            )
        if prefix.get("digest") != recomputed_chain_hash:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.hash_chain_prefix.digest",
                "hash chain prefix digest mismatch",
                {
                    "expected": recomputed_chain_hash,
                    "actual": prefix.get("digest"),
                },
            )
        frame = document["frame"]
        if not isinstance(frame, dict):
            raise _snapshot_error(
                expected_step,
                f"{path.name}.frame",
                "snapshot frame must be an object",
            )
        required_frame_fields = (
            "step",
            "state_hash",
            "chain_hash",
            "covers",
            "crack_events",
            "budget_usage",
            "buffer_watermark_bytes",
        )
        for key in required_frame_fields:
            if key not in frame:
                raise _snapshot_error(
                    expected_step,
                    f"{path.name}.frame.{key}",
                    "required snapshot frame field is missing",
                )
        if frame["step"] != step:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.frame.step",
                "frame step does not match snapshot step",
                {"expected": step, "actual": frame["step"]},
            )
        if frame["state_hash"] != recomputed_state_hash:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.frame.state_hash",
                "frame state hash does not match snapshot state hash",
            )
        if frame["chain_hash"] != recomputed_chain_hash:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.frame.chain_hash",
                "frame chain hash does not match snapshot chain hash",
            )
        if not isinstance(frame["budget_usage"], dict):
            raise _snapshot_error(
                expected_step,
                f"{path.name}.frame.budget_usage",
                "frame budget_usage must be an object",
            )
        if not isinstance(frame["buffer_watermark_bytes"], int):
            raise _snapshot_error(
                expected_step,
                f"{path.name}.frame.buffer_watermark_bytes",
                "frame buffer watermark must be an integer",
            )
        try:
            state = ReplayState.from_payload(state_payload)
        except (KeyError, TypeError, ValueError) as error:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.state",
                f"snapshot state fields are invalid: {error}",
            ) from error
        if state.step != step:
            raise _snapshot_error(
                expected_step,
                f"{path.name}.state.step",
                "state step does not match snapshot step",
            )
        state.state_hash = recomputed_state_hash
        state.chain_hash = recomputed_chain_hash
        documents.append(document)
        previous_chain = recomputed_chain_hash
    if not documents:
        return [], ReplayState(step=-1, covers={}), GENESIS_CHAIN_HASH
    return documents, ReplayState.from_payload(documents[-1]["state"]), previous_chain


def rewrite_trace_from_documents(documents: list[dict[str, Any]], trace_path: Path) -> None:
    trace_path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{trace_path.name}.",
        suffix=".tmp",
        dir=trace_path.parent,
    )
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            for document in documents:
                handle.write(canonical_json_bytes(document["frame"]) + b"\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, trace_path)
        _fsync_directory(trace_path.parent)
    except Exception as error:
        try:
            temporary_path.unlink(missing_ok=True)
        except OSError:
            pass
        raise ReplayError(
            E_IO,
            f"failed to rewrite trajectory log: {error}",
            None,
            str(trace_path),
        ) from error


def _fsync_directory(directory: Path) -> None:
    try:
        descriptor = os.open(
            directory,
            os.O_RDONLY | getattr(os, "O_DIRECTORY", 0),
        )
    except OSError:
        return
    try:
        os.fsync(descriptor)
    except OSError:
        pass
    finally:
        os.close(descriptor)

