from __future__ import annotations

import copy
from pathlib import Path
from typing import Any, Iterable

from .canonical import (
    GENESIS_CHAIN_HASH,
    MODEL_VERSION,
    chain_hash,
    compact_json,
    read_json,
    sha256_hex,
    state_hash,
)
from .config import SampleConfig
from .errors import BreconError
from .io_utils import write_json_atomic


def _copy(value: Any) -> Any:
    return copy.deepcopy(value)


def _nearest_rank(values: list[int], percentile: float) -> int:
    if not values:
        return 0
    ordered = sorted(values)
    index = max(0, int(-(-len(ordered) * percentile // 100)) - 1)
    return ordered[min(index, len(ordered) - 1)]


def _latency_distribution(records: list[dict[str, Any]]) -> dict[str, Any]:
    values = [int(record["end_to_end_delay_ms"]) for record in records]
    if not values:
        return {
            "count": 0,
            "min_ms": 0,
            "max_ms": 0,
            "mean_ms": 0,
            "p50_ms": 0,
            "p95_ms": 0,
            "p99_ms": 0,
        }
    return {
        "count": len(values),
        "min_ms": min(values),
        "max_ms": max(values),
        "mean_ms": round(sum(values) / len(values), 3),
        "p50_ms": _nearest_rank(values, 50),
        "p95_ms": _nearest_rank(values, 95),
        "p99_ms": _nearest_rank(values, 99),
    }


class ReplayEngine:
    """Deterministic room economy replay.

    The engine only uses input timestamps. It never reads the system clock.
    Operations are pre-deducted into holds, grouped by player, and settled at
    fixed observation boundaries. Revocation intents are applied before claims
    in a settlement batch so a client-side cancellation can never create a
    phantom resource; both phases themselves are stable by ``arrival_seq``.
    """

    def __init__(
        self,
        config: SampleConfig,
        operations: Iterable[dict[str, Any]],
        *,
        write_snapshots: bool = False,
    ) -> None:
        self.config = config
        self.write_snapshots = write_snapshots
        self.raw_operations = [dict(operation) for operation in operations]
        self.operations = self._canonicalize_operations(self.raw_operations)
        self._attach_snapshot_interrupt: Any | None = None

        self.claims: dict[tuple[str, int], dict[str, Any]] = {}
        self.revokes: dict[tuple[str, int], dict[str, Any]] = {}
        for operation in self.operations:
            key = (operation["player_id"], operation["client_seq"])
            if operation["op_type"] == "claim":
                self.claims[key] = operation
            else:
                self.revokes[key] = operation

        self.state = self._initial_state()
        self.slot_index = {
            (box["box_id"], slot["slot_id"]): slot
            for box in self.state["boxes"]
            for slot in box["slots"]
        }
        self.inventory_index = {
            player["player_id"]: {
                item["item"]: item for item in player["items"]
            }
            for player in self.state["inventory"]
        }
        self.client_view = {
            player: {item["item"]: int(item["quantity"]) for item in entry["items"]}
            for player, entry in (
                (entry["player_id"], entry) for entry in self.state["inventory"]
            )
        }

        self.remaining: list[dict[str, Any]] = list(self.operations)
        self.revoke_intents = self._revoke_intents()
        self.revoked_keys: set[tuple[str, int]] = set()
        self.revocation_targets: set[tuple[str, int]] = set()
        self.settled_claims: set[tuple[str, int]] = set()
        self.outcome_by_key: dict[tuple[str, int], str] = {}
        self.current_step = self.config.observation.start_step - 1
        self.exception_keys: set[tuple[str, int]] = set()

        self.chain: list[dict[str, Any]] = []
        self.previous_chain_hash = GENESIS_CHAIN_HASH
        self.trace_entries: list[dict[str, Any]] = []
        self.state_history: list[dict[str, Any]] = []
        self.reconciliations: list[dict[str, Any]] = []
        self.runtime_rejections: list[dict[str, Any]] = []
        self.latency_records: list[dict[str, Any]] = []
        self.sync_observations: list[dict[str, Any]] = []
        self.divergences: list[dict[str, Any]] = []
        self.compensation_attempts = 0
        self.compensation_hits = 0
        self.late_records = 0
        self.ordering_events: list[dict[str, Any]] = []
        self.max_arrival_seq = max(
            (int(operation["arrival_seq"]) for operation in self.operations),
            default=-1,
        )
        next_client_seq: dict[str, int] = {}
        for operation in self.operations:
            player = operation["player_id"]
            next_client_seq[player] = max(
                next_client_seq.get(player, 0), int(operation["client_seq"])
            )
        self.next_client_seq = next_client_seq

    @classmethod
    def from_sample_dir(
        cls,
        sample_dir: str | Path,
        *,
        write_snapshots: bool = False,
    ) -> "ReplayEngine":
        config = SampleConfig.load(sample_dir)
        operations = cls._load_operations(config)
        return cls(config, operations, write_snapshots=write_snapshots)

    @classmethod
    def from_snapshot(
        cls,
        config: SampleConfig,
        operations: Iterable[dict[str, Any]],
        snapshot: dict[str, Any],
        *,
        write_snapshots: bool = False,
    ) -> "ReplayEngine":
        engine = cls(config, operations, write_snapshots=write_snapshots)
        canonical_states = snapshot.get("runtime", {}).get("state_history", [])
        if canonical_states and int(canonical_states[-1]["step"]) == int(snapshot["step"]):
            engine.state = _copy(canonical_states[-1]["state"])
        else:
            engine.state = _copy(snapshot["state"])
        engine.slot_index = {
            (box["box_id"], slot["slot_id"]): slot
            for box in engine.state["boxes"]
            for slot in box["slots"]
        }
        engine.inventory_index = {
            player["player_id"]: {
                item["item"]: item for item in player["items"]
            }
            for player in engine.state["inventory"]
        }
        engine.client_view = {
            entry["player_id"]: {
                item["item"]: int(item["quantity"]) for item in entry["items"]
            }
            for entry in engine.state["inventory"]
        }
        engine.remaining = [_copy(operation) for operation in snapshot["buffer"]]
        engine.current_step = int(snapshot["step"])
        engine.chain = _copy(snapshot["chain"])
        engine.previous_chain_hash = (
            engine.chain[-1]["chain_hash"] if engine.chain else GENESIS_CHAIN_HASH
        )

        runtime = snapshot["runtime"]
        engine.trace_entries = _copy(runtime["trace_steps"])
        engine.state_history = _copy(runtime["state_history"])
        engine.reconciliations = _copy(runtime["reconciliations"])
        engine.runtime_rejections = _copy(runtime["rejections"])
        engine.latency_records = _copy(runtime["latency_records"])
        engine.sync_observations = _copy(runtime["sync_observations"])
        engine.divergences = _copy(runtime["divergences"])
        engine.max_arrival_seq = int(runtime["max_arrival_seq"])
        engine.settled_claims = {
            (str(player), int(sequence))
            for player, sequence in (
                value.rsplit(":", 1) for value in runtime["settled_claims"]
            )
        }
        engine.revoked_keys = {
            (record["player_id"], int(record["target_client_seq"]))
            for record in engine.state["revocations"]
        }
        engine.revocation_targets = set(engine.revoked_keys)
        engine.outcome_by_key = {}
        for step_record in engine.trace_entries:
            for operation in step_record["effective_ops"]:
                key = (operation["player_id"], int(operation["client_seq"]))
                engine.outcome_by_key[key] = operation["outcome"]
        engine.compensation_attempts = sum(
            1 for record in engine.latency_records if record["op_type"] == "revoke"
        )
        engine.compensation_hits = sum(
            1
            for record in engine.latency_records
            if record["op_type"] == "revoke"
            and record["status"] == "revoke_accepted"
        )
        engine.late_records = sum(
            1 for record in engine.latency_records if record["status"] == "late"
        )
        for operation in engine.remaining:
            key = (operation["player_id"], int(operation["client_seq"]))
            engine.exception_keys.add(key)
        engine.revoke_intents = engine._revoke_intents(
            include_serialized_buffer=True
        )
        return engine

    @staticmethod
    def _load_operations(config: SampleConfig) -> list[dict[str, Any]]:
        path = config.operations_path
        if not path.is_file():
            raise BreconError(
                "E_INPUT_INVALID",
                f"operations file not found: {path}",
                field_path="operations",
            )
        try:
            payload = read_json(path)
        except (OSError, ValueError) as exc:
            raise BreconError(
                "E_INPUT_INVALID",
                f"operations file is not valid JSON: {exc}",
                field_path="operations",
            ) from exc
        operations = payload.get("operations") if isinstance(payload, dict) else payload
        if not isinstance(operations, list):
            raise BreconError(
                "E_INPUT_INVALID",
                "operations must be a list",
                field_path="operations",
            )
        return [dict(operation) for operation in operations]

    def _canonicalize_operations(
        self, operations: list[dict[str, Any]]
    ) -> list[dict[str, Any]]:
        ordered = sorted(
            enumerate(operations),
            key=lambda pair: (int(pair[1].get("arrival_seq", pair[0])), pair[0]),
        )
        seen: set[tuple[str, int]] = set()
        result: list[dict[str, Any]] = []
        for _, operation in ordered:
            operation_type = operation.get("op_type")
            if operation_type not in {"claim", "revoke"}:
                raise BreconError(
                    "E_INPUT_INVALID",
                    f"unsupported operation type: {operation_type}",
                    field_path="operations.op_type",
                )
            player = str(operation["player_id"])
            client_seq = int(operation["client_seq"])
            key = (player, client_seq)
            if key in seen:
                continue
            seen.add(key)
            arrival_ts = int(operation["arrival_ts_ms"])
            canonical: dict[str, Any] = {
                "op_type": operation_type,
                "player_id": player,
                "client_seq": client_seq,
                "local_ts_ms": int(operation["local_ts_ms"]),
                "arrival_ts_ms": arrival_ts,
                "arrival_seq": int(operation.get("arrival_seq", len(result))),
                "arrival_step": max(
                    1,
                    (
                        arrival_ts
                        - self.config.origin_ts_ms
                        + self.config.step_ms
                        - 1
                    )
                    // self.config.step_ms,
                ),
            }
            if operation_type == "claim":
                canonical.update(
                    {
                        "box_id": str(operation["box_id"]),
                        "slot_id": str(operation["slot_id"]),
                        "item": str(operation["item"]),
                        "quantity": int(operation["quantity"]),
                        "voucher_id": str(operation["voucher_id"]),
                    }
                )
                if canonical["quantity"] <= 0:
                    raise BreconError(
                        "E_INPUT_INVALID",
                        "claim quantity must be positive",
                        field_path=f"operations[{client_seq}].quantity",
                    )
            else:
                canonical["target_client_seq"] = int(operation["target_client_seq"])
            result.append(canonical)
        return result

    def _items(self) -> list[str]:
        items = {
            str(slot["item"])
            for box in self.config.boxes
            for slot in box["slots"]
        }
        for quantities in self.config.initial_inventory.values():
            items.update(quantities)
        return sorted(items)

    def _initial_state(self) -> dict[str, Any]:
        boxes = []
        for box in self.config.boxes:
            slots = []
            for source in box["slots"]:
                slots.append(
                    {
                        "slot_id": str(source["slot_id"]),
                        "item": str(source["item"]),
                        "quantity": int(source["quantity"]),
                        "available": True,
                        "claimed_by": None,
                        "claimed_client_seq": None,
                    }
                )
            boxes.append({"box_id": str(box["box_id"]), "slots": slots})

        items = self._items()
        inventory = []
        for player in self.config.players:
            initial = self.config.initial_inventory.get(player, {})
            inventory.append(
                {
                    "player_id": player,
                    "items": [
                        {"item": item, "quantity": int(initial.get(item, 0))}
                        for item in items
                    ],
                }
            )

        intents = self._revoke_intents()
        holds = []
        for claim in sorted(
            self.claims.values(),
            key=lambda operation: (
                operation["player_id"],
                int(operation["client_seq"]),
            ),
        ):
            key = (claim["player_id"], int(claim["client_seq"]))
            pending = intents.get(key)
            holds.append(
                {
                    "player_id": claim["player_id"],
                    "client_seq": int(claim["client_seq"]),
                    "voucher_id": claim["voucher_id"],
                    "box_id": claim["box_id"],
                    "slot_id": claim["slot_id"],
                    "item": claim["item"],
                    "quantity": int(claim["quantity"]),
                    "revoke_pending_seq": pending,
                }
            )
        return {
            "boxes": boxes,
            "inventory": inventory,
            "holds": holds,
            "revocations": [],
        }

    def _revoke_intents(
        self, *, include_serialized_buffer: bool = False
    ) -> dict[tuple[str, int], int]:
        intents: dict[tuple[str, int], int] = {}
        candidates = list(self.revokes.values())
        if include_serialized_buffer:
            for operation in self.remaining:
                if operation["op_type"] != "revoke":
                    continue
                key = (operation["player_id"], int(operation["client_seq"]))
                candidates.append(operation)
        for revoke in sorted(candidates, key=lambda item: int(item["arrival_seq"])):
            target = (revoke["player_id"], int(revoke["target_client_seq"]))
            if target in self.claims and target not in intents:
                intents[target] = int(revoke["client_seq"])
        return intents

    def _remove_hold(self, key: tuple[str, int]) -> dict[str, Any] | None:
        for index, hold in enumerate(self.state["holds"]):
            if (hold["player_id"], int(hold["client_seq"])) == key:
                return self.state["holds"].pop(index)
        return None

    def _find_hold(self, key: tuple[str, int]) -> dict[str, Any] | None:
        for hold in self.state["holds"]:
            if (hold["player_id"], int(hold["client_seq"])) == key:
                return hold
        return None

    def _settle_time(self, step: int) -> int:
        return self.config.origin_ts_ms + step * self.config.step_ms

    def _client_pre_deduct(self, operation: dict[str, Any]) -> None:
        quantities = self.client_view.setdefault(operation["player_id"], {})
        quantities[operation["item"]] = (
            int(quantities.get(operation["item"], 0)) + int(operation["quantity"])
        )

    def _client_revert(self, operation: dict[str, Any]) -> None:
        quantities = self.client_view.setdefault(operation["player_id"], {})
        quantities[operation["item"]] = (
            int(quantities.get(operation["item"], 0)) - int(operation["quantity"])
        )

    def _effective_record(
        self, operation: dict[str, Any], outcome: str
    ) -> dict[str, Any]:
        return {
            "op_type": operation["op_type"],
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "arrival_seq": int(operation["arrival_seq"]),
            "outcome": outcome,
        }

    def _claim_reconciliation(
        self,
        operation: dict[str, Any],
        result: str,
        reason: str,
        **extra: Any,
    ) -> dict[str, Any]:
        record: dict[str, Any] = {
            "step": self.current_step,
            "op_type": "claim",
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "arrival_seq": int(operation["arrival_seq"]),
            "voucher_id": operation["voucher_id"],
            "box_id": operation["box_id"],
            "slot_id": operation["slot_id"],
            "item": operation["item"],
            "quantity": int(operation["quantity"]),
            "result": result,
            "reason": reason,
        }
        record.update(extra)
        return record

    def _latency_record(
        self,
        operation: dict[str, Any],
        step: int,
        status: str,
    ) -> dict[str, Any]:
        settle_ts = self._settle_time(step)
        return {
            "op_type": operation["op_type"],
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "arrival_seq": int(operation["arrival_seq"]),
            "local_ts_ms": int(operation["local_ts_ms"]),
            "arrival_ts_ms": int(operation["arrival_ts_ms"]),
            "settle_ts_ms": settle_ts,
            "wire_delay_ms": int(operation["arrival_ts_ms"])
            - int(operation["local_ts_ms"]),
            "end_to_end_delay_ms": settle_ts - int(operation["local_ts_ms"]),
            "status": status,
        }

    def _late_rejection(
        self, operation: dict[str, Any], step: int
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        delay = self._settle_time(step) - int(operation["local_ts_ms"])
        message = (
            "operation arrived outside compensation window: "
            f"delay={delay}ms window={self.config.compensation_window_ms}ms"
        )
        details = {
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "arrival_seq": int(operation["arrival_seq"]),
            "delay_ms": delay,
            "window_ms": self.config.compensation_window_ms,
        }
        runtime = {
            "code": "E_OP_LATE",
            "message": message,
            "step": step,
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "target_client_seq": (
                int(operation["target_client_seq"])
                if operation["op_type"] == "revoke"
                else None
            ),
            "arrival_seq": int(operation["arrival_seq"]),
            "details": details,
        }
        trace = {
            "code": "E_OP_LATE",
            "message": message,
            "step": step,
            "field_path": "operations.local_ts_ms",
            "details": details,
        }
        return runtime, trace

    def _conflict_rejection(
        self, operation: dict[str, Any], step: int, actual_item: str
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        message = (
            "pre-deduction conflicts with authoritative inventory: item_mismatch"
        )
        details = {
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "box_id": operation["box_id"],
            "slot_id": operation["slot_id"],
            "reason": "item_mismatch",
            "voucher_id": operation["voucher_id"],
            "expected_item": operation["item"],
            "actual_item": actual_item,
        }
        runtime = {
            "code": "E_CREDIT_CONFLICT",
            "message": message,
            "step": step,
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "target_client_seq": None,
            "arrival_seq": None,
            "details": details,
        }
        trace = {
            "code": "E_CREDIT_CONFLICT",
            "message": message,
            "step": step,
            "field_path": "holds",
            "details": details,
        }
        return runtime, trace

    def _closed_rejection(
        self, operation: dict[str, Any], step: int
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        message = (
            "revoke target is already settled or no longer revocable: "
            f"target_client_seq={operation['target_client_seq']}"
        )
        details = {
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "target_client_seq": int(operation["target_client_seq"]),
            "arrival_seq": int(operation["arrival_seq"]),
        }
        runtime = {
            "code": "E_REVOKE_CLOSED",
            "message": message,
            "step": step,
            "player_id": operation["player_id"],
            "client_seq": int(operation["client_seq"]),
            "target_client_seq": int(operation["target_client_seq"]),
            "arrival_seq": int(operation["arrival_seq"]),
            "details": details,
        }
        trace = {
            "code": "E_REVOKE_CLOSED",
            "message": message,
            "step": step,
            "field_path": "operations.target_client_seq",
            "details": details,
        }
        return runtime, trace

    def _claim_outcome(
        self,
        operation: dict[str, Any],
        step: int,
        runtime_rejections: list[dict[str, Any]],
        trace_rejections: list[dict[str, Any]],
        pending_revoke_sequence: dict[tuple[str, int], int],
    ) -> tuple[str, dict[str, Any] | None]:
        key = (operation["player_id"], int(operation["client_seq"]))
        self._client_pre_deduct(operation)
        slot = self.slot_index.get((operation["box_id"], operation["slot_id"]))
        if key in self.revoked_keys:
            self._client_revert(operation)
            self._remove_hold(key)
            self.settled_claims.add(key)
            self.outcome_by_key[key] = "returned"
            revoke_sequence = pending_revoke_sequence.get(key)
            if revoke_sequence is None:
                revocation = self._target_revocation(key)
                revoke_sequence = (
                    int(revocation["revoke_client_seq"])
                    if revocation is not None
                    else None
                )
            reconciliation = self._claim_reconciliation(
                operation,
                "returned",
                "revoked_before_settlement",
                revoke_client_seq=revoke_sequence,
            )
            return "returned", reconciliation
        if slot is None:
            self._client_revert(operation)
            self._remove_hold(key)
            self.settled_claims.add(key)
            self.outcome_by_key[key] = "conflict"
            runtime, trace = self._conflict_rejection(
                operation, step, "<missing-slot>"
            )
            runtime_rejections.append(runtime)
            trace_rejections.append(trace)
            return "conflict", self._claim_reconciliation(
                operation, "conflict", "slot_missing"
            )
        if slot["item"] != operation["item"]:
            self._client_revert(operation)
            self._remove_hold(key)
            self.settled_claims.add(key)
            self.outcome_by_key[key] = "conflict"
            runtime, trace = self._conflict_rejection(
                operation, step, str(slot["item"])
            )
            runtime_rejections.append(runtime)
            trace_rejections.append(trace)
            return "conflict", self._claim_reconciliation(
                operation, "conflict", "item_mismatch"
            )
        if bool(slot["available"]):
            slot["available"] = False
            slot["claimed_by"] = operation["player_id"]
            slot["claimed_client_seq"] = int(operation["client_seq"])
            self.inventory_index[operation["player_id"]][operation["item"]][
                "quantity"
            ] += int(operation["quantity"])
            self._remove_hold(key)
            self.settled_claims.add(key)
            self.outcome_by_key[key] = "effective"
            return "effective", self._claim_reconciliation(
                operation, "effective", "slot_available"
            )

        owner = slot["claimed_by"]
        owner_sequence = slot["claimed_client_seq"]
        self._client_revert(operation)
        self._remove_hold(key)
        self.settled_claims.add(key)
        self.outcome_by_key[key] = "returned"
        return "returned", self._claim_reconciliation(
            operation,
            "returned",
            "slot_unavailable",
            authoritative_owner=owner,
            authoritative_client_seq=owner_sequence,
        )

    def _target_revocation(
        self, target: tuple[str, int]
    ) -> dict[str, Any] | None:
        for record in reversed(self.state["revocations"]):
            if (
                record["player_id"] == target[0]
                and int(record["target_client_seq"]) == target[1]
            ):
                return record
        return None

    def _revoke_outcome(
        self,
        operation: dict[str, Any],
        step: int,
        settled_before: set[tuple[str, int]],
        active_claim_keys: set[tuple[str, int]],
        runtime_rejections: list[dict[str, Any]],
        trace_rejections: list[dict[str, Any]],
    ) -> str | None:
        target = (operation["player_id"], int(operation["target_client_seq"]))
        target_claim = self.claims.get(target)
        target_outcome = self.outcome_by_key.get(target)
        hold = self._find_hold(target)
        if target_claim is None or target in settled_before:
            accepted = False
        elif target in self.revocation_targets:
            accepted = False
        elif target_outcome in {"effective", "conflict"}:
            accepted = False
        elif target_outcome == "returned":
            accepted = target in self.revoked_keys
        else:
            accepted = hold is not None or target in active_claim_keys
        if not accepted:
            runtime, trace = self._closed_rejection(operation, step)
            runtime_rejections.append(runtime)
            trace_rejections.append(trace)
            return "revoke_closed"

        self.revoked_keys.add(target)
        self.revocation_targets.add(target)
        self._remove_hold(target)
        record = {
            "player_id": target_claim["player_id"],
            "target_client_seq": int(target_claim["client_seq"]),
            "revoke_client_seq": int(operation["client_seq"]),
            "voucher_id": target_claim["voucher_id"],
            "box_id": target_claim["box_id"],
            "slot_id": target_claim["slot_id"],
            "item": target_claim["item"],
            "quantity": int(target_claim["quantity"]),
        }
        self.state["revocations"].append(record)
        self.outcome_by_key[(operation["player_id"], int(operation["client_seq"]))] = (
            "revoke_accepted"
        )
        return "revoke_accepted"

    def _sync_difference(self) -> tuple[int, str | None]:
        difference = 0
        source: str | None = None
        for player in self.config.players:
            authority = self.inventory_index[player]
            visible = self.client_view.get(player, {})
            for item in sorted(set(authority) | set(visible)):
                expected = int(authority.get(item, {}).get("quantity", 0))
                actual = int(visible.get(item, 0))
                delta = abs(expected - actual)
                if delta and source is None:
                    source = f"inventory[{player}].items[{item}]"
                difference += delta
        return difference, source

    def step_once(self) -> dict[str, Any] | None:
        if self.current_step >= self.config.observation.end_step:
            return None
        step = self.current_step + 1
        self.current_step = step
        settle_ts = self._settle_time(step)
        due = [
            operation
            for operation in self.remaining
            if int(operation["arrival_step"]) <= step
        ]
        due.sort(key=lambda operation: int(operation["arrival_seq"]))
        buffer_before = len(self.remaining)
        settled_before = set(self.settled_claims)

        late_operations: list[dict[str, Any]] = []
        active_operations: list[dict[str, Any]] = []
        for operation in due:
            if settle_ts - int(operation["local_ts_ms"]) > (
                self.config.compensation_window_ms
            ):
                late_operations.append(operation)
            else:
                active_operations.append(operation)

        late_runtime_rejections: list[dict[str, Any]] = []
        late_trace_rejections: list[dict[str, Any]] = []
        for operation in late_operations:
            key = (operation["player_id"], int(operation["client_seq"]))
            if operation["op_type"] == "claim":
                self._remove_hold(key)
                self.client_view.setdefault(operation["player_id"], {})
            else:
                target = (operation["player_id"], int(operation["target_client_seq"]))
                hold = self._find_hold(target)
                if (
                    hold is not None
                    and hold.get("revoke_pending_seq")
                    == int(operation["client_seq"])
                ):
                    hold["revoke_pending_seq"] = None
                self.compensation_attempts += 1
            self.late_records += 1
            self.latency_records.append(
                self._latency_record(operation, step, "late")
            )
            runtime, trace = self._late_rejection(operation, step)
            late_runtime_rejections.append(runtime)
            late_trace_rejections.append(trace)

        runtime_rejections: list[dict[str, Any]] = []
        trace_rejections: list[dict[str, Any]] = []
        outcomes: dict[tuple[str, int], str] = {}
        revoke_statuses: dict[tuple[str, int], str] = {}
        claim_reconciliations: list[dict[str, Any]] = []
        pre_batch_slot_available: dict[tuple[str, int], bool] = {}
        for operation in active_operations:
            if operation["op_type"] != "revoke":
                continue
            target = (operation["player_id"], int(operation["target_client_seq"]))
            target_claim = self.claims.get(target)
            if target_claim is None:
                continue
            slot = self.slot_index.get(
                (target_claim["box_id"], target_claim["slot_id"])
            )
            pre_batch_slot_available[target] = bool(
                slot is not None and slot["available"]
            )

        active_claim_keys = {
            (operation["player_id"], int(operation["client_seq"]))
            for operation in active_operations
            if operation["op_type"] == "claim"
        }
        due_claim_keys = {
            (operation["player_id"], int(operation["client_seq"]))
            for operation in due
            if operation["op_type"] == "claim"
        }
        active_revokes = [
            operation
            for operation in active_operations
            if operation["op_type"] == "revoke"
        ]
        pending_revoke_sequence: dict[tuple[str, int], int] = {}
        for operation in sorted(
            active_revokes, key=lambda item: int(item["arrival_seq"])
        ):
            target = (operation["player_id"], int(operation["target_client_seq"]))
            if target in settled_before:
                continue
            if target in due_claim_keys and target not in active_claim_keys:
                continue
            hold = self._find_hold(target)
            if hold is None and target not in active_claim_keys:
                continue
            self.revoked_keys.add(target)
            pending_revoke_sequence.setdefault(
                target, int(operation["client_seq"])
            )
            if hold is not None:
                hold["revoke_pending_seq"] = pending_revoke_sequence[target]

        revoke_latency_records: list[dict[str, Any]] = []
        claim_latency_records: list[dict[str, Any]] = []
        for operation in sorted(
            active_operations, key=lambda item: int(item["arrival_seq"])
        ):
            key = (operation["player_id"], int(operation["client_seq"]))
            if operation["op_type"] == "claim":
                outcome, reconciliation = self._claim_outcome(
                    operation,
                    step,
                    runtime_rejections,
                    trace_rejections,
                    pending_revoke_sequence,
                )
                outcomes[key] = outcome
                claim_latency_records.append(
                    self._latency_record(operation, step, outcome)
                )
                if reconciliation is not None:
                    claim_reconciliations.append(reconciliation)
                continue

            status = self._revoke_outcome(
                operation,
                step,
                settled_before,
                active_claim_keys,
                runtime_rejections,
                trace_rejections,
            )
            self.compensation_attempts += 1
            if status == "revoke_accepted":
                self.compensation_hits += 1
            if status is not None:
                revoke_statuses[key] = status
                revoke_latency_records.append(
                    self._latency_record(operation, step, status)
                )
                if status == "revoke_accepted":
                    outcomes[key] = status

        self.latency_records.extend(revoke_latency_records)
        self.latency_records.extend(claim_latency_records)

        for operation in active_revokes:
            target = (operation["player_id"], int(operation["target_client_seq"]))
            target_claim = self.claims.get(target)
            if target_claim is None:
                continue
            actual_claim = outcomes.get(target)
            actual_revoke = revoke_statuses.get(
                (operation["player_id"], int(operation["client_seq"]))
            )
            available = pre_batch_slot_available.get(target, False)
            alternative_claim = "effective" if available else "returned"
            order_sensitive = (
                actual_claim != alternative_claim
                or actual_revoke != "revoke_closed"
            )
            if int(target_claim["arrival_seq"]) < int(operation["arrival_seq"]):
                arrival_order = ["claim", "revoke"]
            else:
                arrival_order = ["revoke", "claim"]
            self.ordering_events.append(
                {
                    "step": step,
                    "player_id": operation["player_id"],
                    "target_client_seq": int(operation["target_client_seq"]),
                    "claim_arrival_seq": int(target_claim["arrival_seq"]),
                    "revoke_arrival_seq": int(operation["arrival_seq"]),
                    "arrival_order": arrival_order,
                    "applied_order": arrival_order,
                    "settlement_intent_preapplied": True,
                    "order_sensitive": order_sensitive,
                    "slot_available_before_batch": available,
                    "actual_claim_outcome": actual_claim,
                    "actual_revoke_status": actual_revoke,
                    "alternative_if_revoke_intent_absent": {
                        "claim_outcome": alternative_claim,
                        "revoke_status": "revoke_closed",
                    },
                }
            )

        ordered_runtime_rejections = late_runtime_rejections + runtime_rejections
        trace_rejections.extend(late_trace_rejections)

        due_ids = {id(operation) for operation in due}
        self.remaining = [
            operation for operation in self.remaining if id(operation) not in due_ids
        ]
        self.reconciliations.extend(claim_reconciliations)
        self.runtime_rejections.extend(ordered_runtime_rejections)

        effective_ops = [
            self._effective_record(operation, outcomes[key])
            for operation in active_operations
            for key in [(operation["player_id"], int(operation["client_seq"]))]
            if key in outcomes
        ]
        effective_ops.sort(key=lambda operation: int(operation["arrival_seq"]))

        content_hash = state_hash(self.state)
        step_chain_hash = chain_hash(self.previous_chain_hash, content_hash)
        self.previous_chain_hash = step_chain_hash
        chain_entry = {
            "step": step,
            "content_hash": content_hash,
            "prev_chain_hash": (
                GENESIS_CHAIN_HASH
                if not self.chain
                else self.chain[-1]["chain_hash"]
            ),
            "chain_hash": step_chain_hash,
            "buffer_depth": len(self.remaining),
        }
        self.chain.append(chain_entry)

        sync_record: dict[str, Any] | None = None
        if step in self.config.observation.sync_steps:
            difference, source = self._sync_difference()
            previous_sync = (
                self.sync_observations[-1]["step"]
                if self.sync_observations
                else self.config.observation.start_step - 1
            )
            sync_record = {
                "step": step,
                "window_start_step": previous_sync + 1,
                "window_end_step": step,
                "difference": difference,
                "tolerance": self.config.observation.tolerance,
                "status": (
                    "ok"
                    if difference <= self.config.observation.tolerance
                    else "diverged"
                ),
                "source": source,
            }
            self.sync_observations.append(_copy(sync_record))
            if difference > self.config.observation.tolerance:
                details = {
                    "window_start_step": sync_record["window_start_step"],
                    "window_end_step": step,
                    "difference": difference,
                    "tolerance": self.config.observation.tolerance,
                    "source": source,
                }
                divergence = {
                    "code": "E_LATENCY_DIVERGE",
                    "step": step,
                    **details,
                }
                self.divergences.append(divergence)
                message = (
                    "client pre-deduction and authoritative inventory diverged "
                    f"in window {details['window_start_step']}-{step}"
                )
                self.runtime_rejections.append(
                    {
                        "code": "E_LATENCY_DIVERGE",
                        "message": message,
                        "step": step,
                        "player_id": None,
                        "client_seq": None,
                        "target_client_seq": None,
                        "arrival_seq": None,
                        "details": details,
                    }
                )
                trace_rejections.append(
                    {
                        "code": "E_LATENCY_DIVERGE",
                        "message": message,
                        "step": step,
                        "field_path": source,
                        "details": details,
                    }
                )

        trace_entry = {
            "step": step,
            "effective_ops": effective_ops,
            "reconciliations": claim_reconciliations,
            "rejections": trace_rejections,
            "state_hash": content_hash,
            "chain_hash": step_chain_hash,
            "buffer_before": buffer_before,
            "buffer_after": len(self.remaining),
            "buffer_depth": len(self.remaining),
            "pending_holds": len(self.state["holds"]),
            "sync": sync_record,
        }
        self.trace_entries.append(trace_entry)
        self.state_history.append({"step": step, "state": _copy(self.state)})

        if self.write_snapshots:
            self.write_snapshot()
        return trace_entry

    def run(self) -> "ReplayEngine":
        while self.current_step < self.config.observation.end_step:
            self.step_once()
        return self

    def inject_revoke(
        self,
        *,
        player_id: str,
        target_client_seq: int,
        local_ts_ms: int | None = None,
        arrival_ts_ms: int | None = None,
    ) -> dict[str, Any]:
        if player_id not in self.config.players:
            raise BreconError(
                "E_INPUT_INVALID",
                f"unknown player: {player_id}",
                field_path="player_id",
            )
        target = (player_id, int(target_client_seq))
        if target not in self.claims:
            raise BreconError(
                "E_REVOKE_CLOSED",
                "revoke target is not a known pending claim",
                field_path="target_client_seq",
            )
        next_step = min(
            self.config.observation.end_step,
            max(self.current_step + 1, 1),
        )
        if arrival_ts_ms is None:
            arrival_ts_ms = self._settle_time(next_step) - 5
        if local_ts_ms is None:
            local_ts_ms = int(arrival_ts_ms) - 4
        sequence = self.next_client_seq.get(player_id, 0) + 1
        self.next_client_seq[player_id] = sequence
        self.max_arrival_seq += 1
        operation = {
            "op_type": "revoke",
            "player_id": player_id,
            "client_seq": sequence,
            "local_ts_ms": int(local_ts_ms),
            "arrival_ts_ms": int(arrival_ts_ms),
            "arrival_seq": self.max_arrival_seq,
            "arrival_step": max(
                1,
                (
                    int(arrival_ts_ms)
                    - self.config.origin_ts_ms
                    + self.config.step_ms
                    - 1
                )
                // self.config.step_ms,
            ),
            "target_client_seq": int(target_client_seq),
        }
        self.revokes[(player_id, sequence)] = operation
        self.remaining.append(operation)
        hold = self._find_hold(target)
        if hold is not None:
            hold["revoke_pending_seq"] = sequence
        return operation

    def latency_payload(self) -> dict[str, Any]:
        ordered = sorted(
            self.latency_records, key=lambda record: int(record["arrival_seq"])
        )
        hit_rate = (
            round(self.compensation_hits / self.compensation_attempts, 6)
            if self.compensation_attempts
            else 0.0
        )
        return {
            "operations": _copy(ordered),
            "distribution": _latency_distribution(ordered),
            "compensation": {
                "attempts": self.compensation_attempts,
                "hits": self.compensation_hits,
                "hit_rate": hit_rate,
            },
            "sync_observations": _copy(self.sync_observations),
            "divergences": _copy(self.divergences),
        }

    def baseline_payload(self) -> dict[str, Any]:
        steps = []
        for trace, history in zip(self.trace_entries, self.state_history):
            steps.append(
                {
                    "step": trace["step"],
                    "state_hash": trace["state_hash"],
                    "chain_hash": trace["chain_hash"],
                    "effective_ops": _copy(trace["effective_ops"]),
                    "reconciliations": _copy(trace["reconciliations"]),
                    "buffer_depth": trace["buffer_depth"],
                    "buffer_before": trace["buffer_before"],
                    "buffer_after": trace["buffer_after"],
                    "state": _copy(history["state"]),
                }
            )
        return {
            "baseline_version": 1,
            "model_version": MODEL_VERSION,
            "room_id": self.config.room_id,
            "config_digest": self.config.digest,
            "steps": steps,
        }

    def _buffer_records(self) -> list[dict[str, Any]]:
        return [
            _copy(operation)
            for operation in sorted(
                self.remaining,
                key=lambda item: (item["player_id"], int(item["client_seq"])),
            )
        ]

    def _snapshot_state_view(
        self, state: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        """Return the enriched state projection persisted with a snapshot.

        The canonical state used by the hash chain is retained verbatim in
        ``runtime.state_history``. The top-level projection carries timing
        metadata for operators and GUI rendering.
        """

        view = _copy(self.state if state is None else state)
        for hold in view["holds"]:
            key = (hold["player_id"], int(hold["client_seq"]))
            claim = self.claims.get(key)
            if claim is not None:
                hold["local_ts_ms"] = int(claim["local_ts_ms"])
                hold["arrival_ts_ms"] = int(claim["arrival_ts_ms"])
                hold["arrival_step"] = int(claim["arrival_step"])
                hold["arrival_seq"] = int(claim["arrival_seq"])
            pending = hold.get("revoke_pending_seq")
            revoke = (
                self.revokes.get((hold["player_id"], int(pending)))
                if pending is not None
                else None
            )
            hold["revoke_arrival_step"] = (
                int(revoke["arrival_step"]) if revoke is not None else None
            )
        for record in view["revocations"]:
            revoke = self.revokes.get(
                (record["player_id"], int(record["revoke_client_seq"]))
            )
            record["arrival_seq"] = (
                int(revoke["arrival_seq"]) if revoke is not None else None
            )
        return view

    def _runtime_payload(self) -> dict[str, Any]:
        seen = sorted(
            [
                [operation["player_id"], int(operation["client_seq"]), int(operation["arrival_seq"])]
                for operation in self.operations
            ]
            + [
                [operation["player_id"], int(operation["client_seq"]), int(operation["arrival_seq"])]
                for operation in self.remaining
                if (operation["player_id"], int(operation["client_seq"]))
                not in {
                    (item["player_id"], int(item["client_seq"]))
                    for item in self.operations
                }
            ],
            key=lambda item: (item[0], item[1], item[2]),
        )
        return {
            "seen": seen,
            "settled_claims": sorted(
                f"{player}:{sequence}"
                for player, sequence in self.settled_claims
            ),
            "trace_steps": _copy(self.trace_entries),
            "state_history": _copy(self.state_history),
            "reconciliations": _copy(self.reconciliations),
            "rejections": _copy(self.runtime_rejections),
            "latency_records": _copy(self.latency_records),
            "sync_observations": _copy(self.sync_observations),
            "divergences": _copy(self.divergences),
            "max_arrival_seq": self.max_arrival_seq,
        }

    def snapshot_payload(self) -> dict[str, Any]:
        chain_prefix_digest = sha256_hex(compact_json(self.chain))
        payload: dict[str, Any] = {
            "snapshot_version": 1,
            "model_version": MODEL_VERSION,
            "room_id": self.config.room_id,
            "config_digest": self.config.digest,
            "step": self.current_step,
            "buffer_depth": len(self.remaining),
            "state": self._snapshot_state_view(),
            "buffer": self._buffer_records(),
            "chain": _copy(self.chain),
            "chain_prefix_digest": chain_prefix_digest,
            "runtime": self._runtime_payload(),
        }
        payload["checksum"] = sha256_hex(compact_json(payload))
        return payload

    def snapshot_path(self, step: int | None = None) -> Path:
        value = self.current_step if step is None else step
        return self.config.snapshots_path / f"step_{value:06d}.json"

    def write_snapshot(self) -> Path:
        path = self.snapshot_path()
        write_json_atomic(path, self.snapshot_payload())
        write_json_atomic(self.config.snapshots_path / "latest.json", self.snapshot_payload())
        return path

    def write_outputs(self, *, include_baseline: bool = False) -> dict[str, str]:
        from .io_utils import write_json_atomic, write_json_lines_atomic

        write_json_lines_atomic(self.config.trace_path, self.trace_entries)
        write_json_atomic(self.config.latency_path, self.latency_payload())
        if include_baseline:
            write_json_atomic(self.config.baseline_path, self.baseline_payload())
        if not self.write_snapshots:
            for entry, history in zip(self.trace_entries, self.state_history):
                step = int(entry["step"])
                snapshot = self._snapshot_for_step(step, history["state"])
                write_json_atomic(self.snapshot_path(step), snapshot)
            if self.current_step >= 0:
                write_json_atomic(
                    self.config.snapshots_path / "latest.json",
                    self.snapshot_payload(),
                )
        return {
            "trace": str(self.config.trace_path),
            "latency": str(self.config.latency_path),
            "baseline": str(self.config.baseline_path) if include_baseline else "",
            "snapshots": str(self.config.snapshots_path),
        }

    def _snapshot_for_step(
        self, step: int, state: dict[str, Any]
    ) -> dict[str, Any]:
        # Full runs normally write incrementally. This reconstruction path is
        # used by the CLI when replaying silently and then materializing output.
        if step == self.current_step:
            return self.snapshot_payload()
        offset = max(
            0, step - self.config.observation.start_step + 1
        )
        chain = _copy(self.chain[:offset])
        trace_steps = _copy(self.trace_entries[:offset])
        state_history = _copy(self.state_history[:offset])
        latency_records = [
            record
            for record in self.latency_records
            if int(record["settle_ts_ms"])
            <= self._settle_time(step)
        ]
        reconciliations = [
            record
            for record in self.reconciliations
            if int(record["step"]) <= step
        ]
        runtime_rejections = [
            record
            for record in self.runtime_rejections
            if int(record["step"]) <= step
        ]
        sync_observations = [
            record
            for record in self.sync_observations
            if int(record["step"]) <= step
        ]
        divergences = [
            record
            for record in self.divergences
            if int(record["step"]) <= step
        ]
        settled = []
        for trace in trace_steps:
            for operation in trace["effective_ops"]:
                if operation["op_type"] == "claim":
                    settled.append(
                        f"{operation['player_id']}:{operation['client_seq']}"
                    )
        seen = [
            [operation["player_id"], int(operation["client_seq"]), int(operation["arrival_seq"])]
            for operation in self.operations
        ]
        buffer = [
            _copy(operation)
            for operation in self.operations
            if int(operation["arrival_step"]) > step
        ]
        buffer.sort(
            key=lambda item: (item["player_id"], int(item["client_seq"]))
        )
        payload: dict[str, Any] = {
            "snapshot_version": 1,
            "model_version": MODEL_VERSION,
            "room_id": self.config.room_id,
            "config_digest": self.config.digest,
            "step": step,
            "buffer_depth": len(buffer),
            "state": self._snapshot_state_view(state),
            "buffer": buffer,
            "chain": chain,
            "chain_prefix_digest": sha256_hex(compact_json(chain)),
            "runtime": {
                "seen": sorted(seen, key=lambda item: (item[0], item[1], item[2])),
                "settled_claims": sorted(set(settled)),
                "trace_steps": trace_steps,
                "state_history": state_history,
                "reconciliations": reconciliations,
                "rejections": runtime_rejections,
                "latency_records": latency_records,
                "sync_observations": sync_observations,
                "divergences": divergences,
                "max_arrival_seq": self.max_arrival_seq,
            },
        }
        payload["checksum"] = sha256_hex(compact_json(payload))
        return payload

    def replay_signature(self) -> dict[str, Any]:
        return {
            "trace_bytes": "".join(
                compact_json(entry) + "\n" for entry in self.trace_entries
            ).encode("utf-8"),
            "latency_bytes": (compact_json(self.latency_payload()) + "\n").encode(
                "utf-8"
            ),
            "head_state_hash": (
                self.trace_entries[-1]["state_hash"] if self.trace_entries else None
            ),
            "head_chain_hash": (
                self.trace_entries[-1]["chain_hash"] if self.trace_entries else None
            ),
        }

    def conservation_report(self) -> dict[str, Any]:
        expected: dict[str, dict[str, int]] = {}
        for player in self.config.players:
            expected[player] = {
                item: int(quantity)
                for item, quantity in self.config.initial_inventory.get(
                    player, {}
                ).items()
            }
        for box in self.state["boxes"]:
            for slot in box["slots"]:
                if not slot["available"] and slot["claimed_by"] is not None:
                    quantities = expected.setdefault(slot["claimed_by"], {})
                    quantities[slot["item"]] = quantities.get(
                        slot["item"], 0
                    ) + int(slot["quantity"])
        actual = {
            entry["player_id"]: {
                item["item"]: int(item["quantity"]) for item in entry["items"]
            }
            for entry in self.state["inventory"]
        }
        differences = []
        for player in self.config.players:
            for item in sorted(set(expected.get(player, {})) | set(actual.get(player, {}))):
                left = int(expected.get(player, {}).get(item, 0))
                right = int(actual.get(player, {}).get(item, 0))
                if left != right:
                    differences.append(
                        {
                            "player_id": player,
                            "item": item,
                            "expected": left,
                            "actual": right,
                        }
                    )
        return {
            "ok": not differences,
            "differences": differences,
            "total_slots": sum(
                len(box["slots"]) for box in self.state["boxes"]
            ),
            "claimed_slots": sum(
                1
                for box in self.state["boxes"]
                for slot in box["slots"]
                if not slot["available"]
            ),
        }

