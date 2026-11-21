from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Mapping

from .errors import BusinessError, E_RECOVERY_INVALID
from .hashing import content_hash, hash_without, pretty_json_bytes, write_bytes_atomic
from .models import LayoutParams, SCHEMA_STATE, SubtitleAsset
from .layout import ZERO_HASH


STATE_FILE = "layout_state.json"


def _recovery_error(message: str, **context: Any) -> BusinessError:
    return BusinessError(E_RECOVERY_INVALID, message, context)


def _required(value: Mapping[str, Any], key: str, *, field_path: str) -> Any:
    if key not in value:
        raise _recovery_error("required state field is missing", field=field_path)
    return value[key]


def verify_report(report: Mapping[str, Any], *, expected_asset_id: str | None = None) -> None:
    if not isinstance(report, Mapping):
        raise _recovery_error("report must be an object")
    for key in (
        "asset_id",
        "commit_hash",
        "descriptions",
        "params",
        "parent_hash",
        "revision",
        "schema",
        "sentences",
        "speaker_markers",
    ):
        _required(report, key, field_path=f"report.{key}")
    if report["schema"] != "layout-report/v1":
        raise _recovery_error("unexpected report schema", field="report.schema", value=report["schema"])
    if expected_asset_id is not None and report["asset_id"] != expected_asset_id:
        raise _recovery_error(
            "report asset id does not match the state",
            field="report.asset_id",
            value=report["asset_id"],
        )
    actual_commit = hash_without(report, "commit_hash")
    if actual_commit != report["commit_hash"]:
        raise _recovery_error(
            "report content hash mismatch",
            field="report.commit_hash",
            expected=actual_commit,
            actual=report["commit_hash"],
        )
    for index, entry in enumerate(report["sentences"]):
        if not isinstance(entry, Mapping) or "entry_hash" not in entry:
            raise _recovery_error("sentence entry hash is missing", field=f"report.sentences[{index}]")
        if hash_without(entry, "entry_hash") != entry["entry_hash"]:
            raise _recovery_error(
                "sentence entry hash mismatch",
                field=f"report.sentences[{index}].entry_hash",
                sentence_id=entry.get("sentence_id"),
            )
    for index, item in enumerate(report["descriptions"]):
        if not isinstance(item, Mapping) or "description_hash" not in item:
            raise _recovery_error("description hash is missing", field=f"report.descriptions[{index}]")
        if hash_without(item, "description_hash") != item["description_hash"]:
            raise _recovery_error(
                "description hash mismatch",
                field=f"report.descriptions[{index}].description_hash",
                description_id=item.get("id"),
            )
    for index, item in enumerate(report["speaker_markers"]):
        if not isinstance(item, Mapping) or "marker_hash" not in item:
            raise _recovery_error("speaker marker hash is missing", field=f"report.speaker_markers[{index}]")
        if hash_without(item, "marker_hash") != item["marker_hash"]:
            raise _recovery_error(
                "speaker marker hash mismatch",
                field=f"report.speaker_markers[{index}].marker_hash",
            )


def verify_pending(pending: Any, *, report_revision: int) -> None:
    if pending is None:
        return
    if not isinstance(pending, Mapping):
        raise _recovery_error("pending journal must be an object or null")
    required = (
        "affected_sentence_ids",
        "base_revision",
        "completed_sentence_ids",
        "computed_entries",
        "operation_hash",
        "params",
        "sentence_updates",
    )
    for key in required:
        _required(pending, key, field_path=f"pending.{key}")
    if pending["base_revision"] != report_revision:
        raise _recovery_error(
            "pending journal does not belong to the committed revision",
            field="pending.base_revision",
            expected=report_revision,
            actual=pending["base_revision"],
        )
    affected = pending["affected_sentence_ids"]
    completed = pending["completed_sentence_ids"]
    computed = pending["computed_entries"]
    if not isinstance(affected, list) or not all(isinstance(item, str) for item in affected):
        raise _recovery_error("invalid affected sentence list", field="pending.affected_sentence_ids")
    if not isinstance(completed, list) or not all(isinstance(item, str) for item in completed):
        raise _recovery_error("invalid completed sentence list", field="pending.completed_sentence_ids")
    if not set(completed).issubset(set(affected)):
        raise _recovery_error(
            "completed checkpoint is outside the affected set",
            field="pending.completed_sentence_ids",
        )
    if not isinstance(computed, Mapping):
        raise _recovery_error("invalid computed entry cache", field="pending.computed_entries")
    for sentence_id, entry in computed.items():
        if sentence_id not in completed:
            raise _recovery_error(
                "computed entry has no completed checkpoint",
                field=f"pending.computed_entries.{sentence_id}",
            )
        if not isinstance(entry, Mapping) or "entry_hash" not in entry:
            raise _recovery_error(
                "computed entry hash is missing",
                field=f"pending.computed_entries.{sentence_id}",
            )
        if hash_without(entry, "entry_hash") != entry["entry_hash"]:
            raise _recovery_error(
                "computed entry hash mismatch",
                field=f"pending.computed_entries.{sentence_id}.entry_hash",
            )
def verify_state(state: Mapping[str, Any]) -> None:
    if not isinstance(state, Mapping):
        raise _recovery_error("state must be an object")
    for key in (
        "asset_id",
        "history",
        "history_hash",
        "inputs",
        "params",
        "pending",
        "pending_hash",
        "report",
        "revision",
        "schema",
        "state_hash",
    ):
        _required(state, key, field_path=key)
    if state["schema"] != SCHEMA_STATE:
        raise _recovery_error("unexpected state schema", field="schema", value=state["schema"])
    asset_id = state["asset_id"]
    if not isinstance(asset_id, str) or not asset_id:
        raise _recovery_error("invalid asset id", field="asset_id")
    report = state["report"]
    verify_report(report, expected_asset_id=asset_id)
    revision = state["revision"]
    if revision != report["revision"]:
        raise _recovery_error(
            "state revision does not match the report",
            field="revision",
            expected=report["revision"],
            actual=revision,
        )
    history = state["history"]
    if not isinstance(history, list) or not history:
        raise _recovery_error("empty hash chain", field="history")
    previous_hash = ZERO_HASH
    for index, record in enumerate(history, 1):
        if not isinstance(record, Mapping):
            raise _recovery_error("hash-chain record must be an object", field=f"history[{index - 1}]")
        for key in ("commit_hash", "parent_hash", "revision"):
            _required(record, key, field_path=f"history[{index - 1}].{key}")
        if record["revision"] != index:
            raise _recovery_error(
                "hash chain revision is not contiguous",
                field=f"history[{index - 1}].revision",
                expected=index,
                actual=record["revision"],
            )
        if record["parent_hash"] != previous_hash:
            raise _recovery_error(
                "hash chain parent link is broken",
                field=f"history[{index - 1}].parent_hash",
                expected=previous_hash,
                actual=record["parent_hash"],
            )
        previous_hash = record["commit_hash"]
    last = history[-1]
    if last["commit_hash"] != report["commit_hash"]:
        raise _recovery_error(
            "hash chain head does not match the report",
            field="history[-1].commit_hash",
            expected=report["commit_hash"],
            actual=last["commit_hash"],
        )
    expected_parent = history[-2]["commit_hash"] if len(history) > 1 else ZERO_HASH
    if report["parent_hash"] != expected_parent:
        raise _recovery_error(
            "report parent hash does not match the chain",
            field="report.parent_hash",
            expected=expected_parent,
            actual=report["parent_hash"],
        )
    if content_hash(history) != state["history_hash"]:
        raise _recovery_error("hash-chain digest mismatch", field="history_hash")
    inputs = state["inputs"]
    if not isinstance(inputs, Mapping):
        raise _recovery_error("inputs must be an object", field="inputs")
    try:
        asset = SubtitleAsset.from_dict(inputs)
    except BusinessError as exc:
        raise _recovery_error(
            "state inputs are invalid",
            field="inputs",
            cause=exc.to_dict(),
        ) from exc
    if asset.asset_id != asset_id:
        raise _recovery_error("state inputs have a different asset id", field="inputs.asset_id")
    try:
        LayoutParams.from_dict(state["params"])
    except BusinessError as exc:
        raise _recovery_error(
            "state parameters are invalid",
            field="params",
            cause=exc.to_dict(),
        ) from exc
    pending = state["pending"]
    verify_pending(pending, report_revision=revision)
    expected_pending_hash = content_hash(pending)
    if state["pending_hash"] != expected_pending_hash:
        raise _recovery_error(
            "top-level pending hash mismatch",
            field="pending_hash",
            expected=expected_pending_hash,
            actual=state["pending_hash"],
        )
    state_payload = {key: value for key, value in state.items() if key != "state_hash"}
    actual_state_hash = content_hash(state_payload)
    if state["state_hash"] != actual_state_hash:
        raise _recovery_error(
            "top-level state hash mismatch",
            field="state_hash",
            expected=actual_state_hash,
            actual=state["state_hash"],
        )


def load_state(path: Path | str) -> dict[str, Any] | None:
    path = Path(path)
    if not path.exists():
        return None
    try:
        with path.open("r", encoding="utf-8") as handle:
            state = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise _recovery_error(
            "cannot read layout state",
            field="layout_state.json",
            detail=str(exc),
        ) from exc
    verify_state(state)
    return state


def build_state(
    *,
    asset: SubtitleAsset,
    params: LayoutParams,
    report: Mapping[str, Any],
    history: list[Mapping[str, Any]],
    pending: Mapping[str, Any] | None,
) -> dict[str, Any]:
    state: dict[str, Any] = {
        "asset_id": asset.asset_id,
        "history": [dict(item) for item in history],
        "history_hash": content_hash(history),
        "inputs": asset.to_dict(),
        "params": params.to_dict(),
        "pending": dict(pending) if pending is not None else None,
        "pending_hash": content_hash(pending),
        "report": dict(report),
        "revision": int(report["revision"]),
        "schema": SCHEMA_STATE,
    }
    state["state_hash"] = content_hash(state)
    return state


def save_state(path: Path | str, state: Mapping[str, Any]) -> None:
    write_bytes_atomic(Path(path), pretty_json_bytes(state))


def save_json_atomic(path: Path | str, value: Any) -> None:
    write_bytes_atomic(Path(path), pretty_json_bytes(value))

