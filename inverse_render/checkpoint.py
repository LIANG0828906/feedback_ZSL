"""Atomic checkpoint commits with a verifiable per-step hash chain."""

from __future__ import annotations

import json
import math
import os
import re
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

from .errors import ErrorCode, ToolError, invalid_checkpoint
from .hashing import GENESIS_HASH, atomic_write_json, canonical_json_bytes, sha256_json


CHECKPOINT_SCHEMA_VERSION = 1
CHECKPOINT_FORMAT = "inverse-render-checkpoint-v1"

_TOP_LEVEL_FIELDS = (
    "schema_version",
    "format",
    "step",
    "scene_hash",
    "graph_version",
    "groups",
    "cumulative_loss",
    "last_loss",
    "node_cache_hashes",
    "previous_hash",
    "history",
    "chain_head",
    "checkpoint_hash",
)

_REQUIRED_CACHE_NODES = (
    "intersections",
    "visibility",
    "shadow_factor",
    "lighting",
    "loss",
)


@dataclass(frozen=True)
class ResumeState:
    step: int
    groups: Mapping[str, Mapping[str, Any]]
    cumulative_loss: float
    last_loss: float
    graph_version: str
    cache_hashes: Mapping[str, str]
    checkpoint_hash: str
    previous_hash: str
    history: tuple[Mapping[str, Any], ...]
    path: str

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("step", self.step),
                ("cumulative_loss", self.cumulative_loss),
                ("last_loss", self.last_loss),
                ("graph_version", self.graph_version),
                ("cache_hashes", OrderedDict(self.cache_hashes)),
                ("checkpoint_hash", self.checkpoint_hash),
                ("previous_hash", self.previous_hash),
                ("path", self.path),
            )
        )


def _is_hex_digest(value: Any) -> bool:
    return isinstance(value, str) and bool(re.fullmatch(r"[0-9a-f]{64}", value))


def _history_record(step: int, payload_hash: str, previous_hash: str) -> OrderedDict[str, Any]:
    base = OrderedDict(
        (
            ("step", int(step)),
            ("payload_hash", payload_hash),
            ("previous_hash", previous_hash),
        )
    )
    result = OrderedDict(base)
    result["record_hash"] = sha256_json(base)
    return result


class CheckpointManager:
    """Commit only complete steps, never exposing a partially written file."""

    def __init__(
        self,
        state_dir: str | Path,
        *,
        scene_hash: str,
        expected_graph_version: str | None = None,
        expected_group_order: Sequence[str] | None = None,
        expected_group_parameters: Mapping[str, Sequence[str]] | None = None,
    ) -> None:
        self.state_dir = Path(state_dir).resolve()
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.path = self.state_dir / "checkpoint.json"
        self.tmp_path = self.state_dir / "checkpoint.json.tmp"
        self.scene_hash = scene_hash
        self.expected_graph_version = expected_graph_version
        self.expected_group_order = (
            tuple(expected_group_order) if expected_group_order is not None else None
        )
        self.expected_group_parameters = (
            OrderedDict(
                (group_id, tuple(paths))
                for group_id, paths in expected_group_parameters.items()
            )
            if expected_group_parameters is not None
            else None
        )
        self._history: list[OrderedDict[str, Any]] = []
        self._last_step: int | None = None
        self._checkpoint_hash: str | None = None

    @property
    def exists(self) -> bool:
        return self.path.exists()

    @property
    def last_committed_step(self) -> int | None:
        return self._last_step

    def _assert_writable(self) -> None:
        try:
            self.state_dir.mkdir(parents=True, exist_ok=True)
            probe = self.state_dir / ".write_probe"
            with open(probe, "wb") as handle:
                handle.write(b"ok")
                handle.flush()
                os.fsync(handle.fileno())
            probe.unlink()
        except OSError as exc:
            raise ToolError(
                ErrorCode.E_IO,
                "state directory is not writable",
                details={"path": str(self.state_dir)},
            ) from exc

    def commit(
        self,
        *,
        step: int,
        groups: Mapping[str, Mapping[str, Any]],
        cumulative_loss: float,
        last_loss: float,
        graph_version: str,
        cache_hashes: Mapping[str, str],
    ) -> str:
        self._assert_writable()
        if isinstance(step, bool) or not isinstance(step, int) or step < 0:
            raise invalid_checkpoint("step must be a non-negative integer", step=step)
        if self._last_step is not None and step != self._last_step + 1:
            raise invalid_checkpoint(
                "checkpoint commits must advance exactly one step",
                step=step,
                expected=self._last_step + 1,
            )
        if self._last_step is None and step != 0 and not self._history:
            raise invalid_checkpoint("first checkpoint commit must be step 0", step=step)
        if not math.isfinite(float(cumulative_loss)) or not math.isfinite(float(last_loss)):
            raise invalid_checkpoint("checkpoint losses must be finite", step=step)
        if not isinstance(graph_version, str) or not graph_version:
            raise invalid_checkpoint("graph_version must be a non-empty string", step=step)
        if self.expected_group_order is not None:
            if tuple(groups) != self.expected_group_order:
                raise invalid_checkpoint(
                    "group order does not match checkpoint configuration",
                    step=step,
                )
        if self.expected_group_parameters is not None:
            for group_id, expected_paths in self.expected_group_parameters.items():
                if group_id not in groups or not isinstance(groups[group_id], dict):
                    raise invalid_checkpoint(
                        "checkpoint is missing a configured parameter group",
                        step=step,
                        group=group_id,
                    )
                if tuple(groups[group_id]) != tuple(expected_paths):
                    raise invalid_checkpoint(
                        "checkpoint parameter paths do not match configuration",
                        step=step,
                        group=group_id,
                        expected=list(expected_paths),
                        actual=list(groups[group_id]),
                    )
        for hash_value in cache_hashes.values():
            if not _is_hex_digest(hash_value):
                raise invalid_checkpoint(
                    "node cache hash is not a SHA-256 digest",
                    step=step,
                )

        previous_hash = (
            self._history[-1]["payload_hash"] if self._history else GENESIS_HASH
        )
        core: OrderedDict[str, Any] = OrderedDict(
            (
                ("schema_version", CHECKPOINT_SCHEMA_VERSION),
                ("format", CHECKPOINT_FORMAT),
                ("step", int(step)),
                ("scene_hash", self.scene_hash),
                ("graph_version", graph_version),
                ("groups", OrderedDict((group_id, OrderedDict(values)) for group_id, values in groups.items())),
                ("cumulative_loss", float(cumulative_loss)),
                ("last_loss", float(last_loss)),
                ("node_cache_hashes", OrderedDict(cache_hashes)),
                ("previous_hash", previous_hash),
            )
        )
        payload_hash = sha256_json(core)
        record = _history_record(step, payload_hash, previous_hash)
        history = list(self._history) + [record]
        document: OrderedDict[str, Any] = OrderedDict(core)
        document["history"] = history
        document["chain_head"] = payload_hash
        checkpoint_hash = sha256_json(document)
        document["checkpoint_hash"] = checkpoint_hash
        atomic_write_json(self.path, document)
        self._history = history
        self._last_step = step
        self._checkpoint_hash = checkpoint_hash
        return checkpoint_hash

    def _read_document(self) -> tuple[OrderedDict[str, Any], int]:
        try:
            data = self.path.read_bytes()
        except FileNotFoundError as exc:
            raise invalid_checkpoint("checkpoint file does not exist", step=-1) from exc
        except OSError as exc:
            raise ToolError(
                ErrorCode.E_IO,
                "cannot read checkpoint",
                details={"path": str(self.path)},
            ) from exc
        step_hint = -1
        match = re.search(rb'"step"\s*:\s*(-?\d+)', data)
        if match:
            step_hint = int(match.group(1))
        try:
            document = json.loads(data.decode("utf-8"), object_pairs_hook=OrderedDict)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise invalid_checkpoint(
                "checkpoint is not valid JSON",
                step=step_hint,
                path=str(self.path),
            ) from exc
        if not isinstance(document, dict):
            raise invalid_checkpoint("checkpoint root must be an object", step=step_hint)
        raw_step = document.get("step", step_hint)
        if isinstance(raw_step, bool) or not isinstance(raw_step, int) or raw_step < 0:
            raise invalid_checkpoint("checkpoint step is invalid", step=step_hint)
        return document, raw_step

    def _validate_history(
        self,
        document: Mapping[str, Any],
        *,
        step: int,
    ) -> tuple[list[OrderedDict[str, Any]], str]:
        history = document.get("history")
        if not isinstance(history, list) or not history:
            raise invalid_checkpoint("checkpoint history is missing or empty", step=step)
        normalized: list[OrderedDict[str, Any]] = []
        previous: str | None = None
        expected_step = 0
        for index, raw_entry in enumerate(history):
            entry_step = (
                raw_entry.get("step", expected_step)
                if isinstance(raw_entry, dict)
                else expected_step
            )
            if not isinstance(raw_entry, dict):
                raise invalid_checkpoint(
                    "checkpoint history entry must be an object", step=entry_step
                )
            for field in ("step", "payload_hash", "previous_hash", "record_hash"):
                if field not in raw_entry:
                    raise invalid_checkpoint(
                        "checkpoint history entry is missing a required field",
                        step=entry_step,
                        field=field,
                        index=index,
                    )
            if raw_entry["step"] != expected_step:
                raise invalid_checkpoint(
                    "checkpoint history step sequence is broken",
                    step=entry_step,
                    expected_step=expected_step,
                )
            payload_hash = raw_entry["payload_hash"]
            prior_hash = raw_entry["previous_hash"]
            record_hash = raw_entry["record_hash"]
            if not _is_hex_digest(payload_hash) or not _is_hex_digest(prior_hash) or not _is_hex_digest(record_hash):
                raise invalid_checkpoint(
                    "checkpoint history contains an invalid digest",
                    step=entry_step,
                )
            computed_record = sha256_json(
                OrderedDict(
                    (
                        ("step", raw_entry["step"]),
                        ("payload_hash", payload_hash),
                        ("previous_hash", prior_hash),
                    )
                )
            )
            if computed_record != record_hash:
                raise invalid_checkpoint(
                    "checkpoint history record hash mismatch",
                    step=entry_step,
                )
            if expected_step == 0:
                if prior_hash != GENESIS_HASH:
                    raise invalid_checkpoint(
                        "checkpoint history genesis link is invalid", step=0
                    )
            elif previous is not None and prior_hash != previous:
                raise invalid_checkpoint(
                    "checkpoint history hash chain is broken",
                    step=entry_step,
                )
            normalized.append(OrderedDict(raw_entry))
            previous = payload_hash
            expected_step += 1
        if normalized[-1]["step"] != step:
            raise invalid_checkpoint(
                "checkpoint step does not match history head",
                step=step,
                history_step=normalized[-1]["step"],
            )
        if previous is None:
            raise invalid_checkpoint("checkpoint hash chain is empty", step=step)
        return normalized, previous

    def load(self) -> ResumeState | None:
        if not self.path.exists():
            return None
        document, step = self._read_document()
        for field in _TOP_LEVEL_FIELDS:
            if field not in document:
                raise invalid_checkpoint(
                    "checkpoint is missing a required field",
                    step=step,
                    field=field,
                )
        if document["schema_version"] != CHECKPOINT_SCHEMA_VERSION:
            raise invalid_checkpoint(
                "unsupported checkpoint schema_version", step=step
            )
        if document["format"] != CHECKPOINT_FORMAT:
            raise invalid_checkpoint("unsupported checkpoint format", step=step)
        if document["scene_hash"] != self.scene_hash:
            raise invalid_checkpoint(
                "checkpoint belongs to a different scene",
                step=step,
                expected_scene_hash=self.scene_hash,
                actual_scene_hash=document["scene_hash"],
            )
        if (
            self.expected_graph_version is not None
            and document["graph_version"] != self.expected_graph_version
        ):
            raise invalid_checkpoint(
                "checkpoint graph version does not match current graph",
                step=step,
                expected_graph_version=self.expected_graph_version,
                actual_graph_version=document["graph_version"],
            )

        groups = document["groups"]
        if not isinstance(groups, dict):
            raise invalid_checkpoint("checkpoint groups must be an object", step=step)
        if self.expected_group_order is not None and tuple(groups) != self.expected_group_order:
            raise invalid_checkpoint(
                "checkpoint group order does not match configuration", step=step
            )
        for group_id, values in groups.items():
            if not isinstance(values, dict) or not values:
                raise invalid_checkpoint(
                    "checkpoint group values are invalid",
                    step=step,
                    group=group_id,
                )
        if self.expected_group_parameters is not None:
            for group_id, expected_paths in self.expected_group_parameters.items():
                if group_id not in groups or not isinstance(groups[group_id], dict):
                    raise invalid_checkpoint(
                        "checkpoint is missing a configured parameter group",
                        step=step,
                        group=group_id,
                    )
                if tuple(groups[group_id]) != tuple(expected_paths):
                    raise invalid_checkpoint(
                        "checkpoint parameter paths do not match configuration",
                        step=step,
                        group=group_id,
                        expected=list(expected_paths),
                        actual=list(groups[group_id]),
                    )
        for loss_field in ("cumulative_loss", "last_loss"):
            value = document[loss_field]
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(float(value)):
                raise invalid_checkpoint(
                    "checkpoint loss is invalid",
                    step=step,
                    field=loss_field,
                )
        cache_hashes = document["node_cache_hashes"]
        if not isinstance(cache_hashes, dict) or any(
            not _is_hex_digest(value) for value in cache_hashes.values()
        ):
            raise invalid_checkpoint(
                "checkpoint node cache hashes are invalid", step=step
            )
        missing_cache_nodes = [node for node in _REQUIRED_CACHE_NODES if node not in cache_hashes]
        if missing_cache_nodes:
            raise invalid_checkpoint(
                "checkpoint is missing node cache hashes",
                step=step,
                nodes=missing_cache_nodes,
            )
        if not isinstance(document["graph_version"], str) or not document["graph_version"]:
            raise invalid_checkpoint("checkpoint graph_version is invalid", step=step)
        if not _is_hex_digest(document["previous_hash"]):
            raise invalid_checkpoint("checkpoint previous_hash is invalid", step=step)
        if not _is_hex_digest(document["chain_head"]):
            raise invalid_checkpoint("checkpoint chain_head is invalid", step=step)
        if not _is_hex_digest(document["checkpoint_hash"]):
            raise invalid_checkpoint("checkpoint checkpoint_hash is invalid", step=step)

        history, history_head = self._validate_history(document, step=step)
        core = OrderedDict((field, document[field]) for field in _TOP_LEVEL_FIELDS[:-3])
        computed_payload = sha256_json(core)
        if computed_payload != history[-1]["payload_hash"]:
            raise invalid_checkpoint(
                "checkpoint payload hash does not match its history entry",
                step=step,
            )
        if document["chain_head"] != history_head:
            raise invalid_checkpoint("checkpoint chain head mismatch", step=step)
        if history[-1]["previous_hash"] != document["previous_hash"]:
            raise invalid_checkpoint(
                "checkpoint previous hash does not match history", step=step
            )
        chain_document = OrderedDict(
            (field, document[field]) for field in _TOP_LEVEL_FIELDS if field != "checkpoint_hash"
        )
        computed_checkpoint_hash = sha256_json(chain_document)
        if computed_checkpoint_hash != document["checkpoint_hash"]:
            raise invalid_checkpoint("checkpoint self-hash mismatch", step=step)

        self._history = history
        self._last_step = step
        self._checkpoint_hash = document["checkpoint_hash"]
        return ResumeState(
            step=step,
            groups=groups,
            cumulative_loss=float(document["cumulative_loss"]),
            last_loss=float(document["last_loss"]),
            graph_version=document["graph_version"],
            cache_hashes=cache_hashes,
            checkpoint_hash=document["checkpoint_hash"],
            previous_hash=document["previous_hash"],
            history=tuple(history),
            path=str(self.path),
        )

    def discard(self) -> None:
        for path in (self.path, self.tmp_path):
            try:
                path.unlink()
            except FileNotFoundError:
                pass
        self._history = []
        self._last_step = None
        self._checkpoint_hash = None

