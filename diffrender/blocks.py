"""Fixed-size screen blocks, serial commit ledger and bounded scheduling."""

from __future__ import annotations

import time
import threading
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any, Callable

import torch

from .errors import AppError, fail
from .serialization import (
    tensor_content_hash,
    tensor_from_payload,
    tensor_to_payload,
)


@dataclass(frozen=True)
class BlockSpec:
    sequence: int
    block_id: str
    image_index: int
    target_id: str
    camera_id: str
    x_offset: int
    y_offset: int
    width: int
    height: int
    shadow_seed: int

    @property
    def pixels(self) -> int:
        return self.width * self.height

    def to_dict(self) -> dict[str, Any]:
        return {
            "sequence": self.sequence,
            "block_id": self.block_id,
            "image_index": self.image_index,
            "target_id": self.target_id,
            "camera_id": self.camera_id,
            "x_offset": self.x_offset,
            "y_offset": self.y_offset,
            "width": self.width,
            "height": self.height,
            "shadow_seed": self.shadow_seed,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "BlockSpec":
        return cls(**payload)


@dataclass
class BlockRecord:
    round_index: int
    sequence: int
    block_id: str
    status: str
    snapshot_hash: str
    parameter_version: int
    shadow_seed: int
    attempts: int
    loss: float | None
    gradient_norm: float | None
    duration_ms: float
    error_code: str | None = None
    error_message: str | None = None
    grad_buffer: torch.Tensor | None = None
    buffer_elements: int = 0

    def to_dict(self, *, include_gradient: bool = True) -> dict[str, Any]:
        payload = {
            "round_index": self.round_index,
            "sequence": self.sequence,
            "block_id": self.block_id,
            "status": self.status,
            "snapshot_hash": self.snapshot_hash,
            "parameter_version": self.parameter_version,
            "shadow_seed": self.shadow_seed,
            "attempts": self.attempts,
            "loss": self.loss,
            "gradient_norm": self.gradient_norm,
            "duration_ms": self.duration_ms,
            "error_code": self.error_code,
            "error_message": self.error_message,
            "buffer_elements": self.buffer_elements,
        }
        if include_gradient and self.grad_buffer is not None:
            payload["grad_buffer"] = tensor_to_payload(self.grad_buffer)
        return payload

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "BlockRecord":
        gradient = payload.get("grad_buffer")
        return cls(
            round_index=int(payload["round_index"]),
            sequence=int(payload["sequence"]),
            block_id=str(payload["block_id"]),
            status=str(payload["status"]),
            snapshot_hash=str(payload["snapshot_hash"]),
            parameter_version=int(payload["parameter_version"]),
            shadow_seed=int(payload["shadow_seed"]),
            attempts=int(payload["attempts"]),
            loss=None if payload.get("loss") is None else float(payload["loss"]),
            gradient_norm=(
                None
                if payload.get("gradient_norm") is None
                else float(payload["gradient_norm"])
            ),
            duration_ms=float(payload.get("duration_ms", 0.0)),
            error_code=payload.get("error_code"),
            error_message=payload.get("error_message"),
            grad_buffer=(
                tensor_from_payload(gradient) if gradient is not None else None
            ),
            buffer_elements=int(payload.get("buffer_elements", 0)),
        )


@dataclass
class RoundState:
    round_index: int
    snapshot_hash: str
    parameter_version: int
    specs: list[BlockSpec]
    snapshot: torch.Tensor | None = None
    committed_count: int = 0
    records: list[BlockRecord] = field(default_factory=list)
    budget_waits: int = 0

    @property
    def committed_ids(self) -> list[str]:
        return [
            record.block_id
            for record in self.records
            if record.status == "committed"
        ]

    def to_dict(self) -> dict[str, Any]:
        return {
            "round_index": self.round_index,
            "snapshot_hash": self.snapshot_hash,
            "parameter_version": self.parameter_version,
            "specs": [spec.to_dict() for spec in self.specs],
            "snapshot": (
                tensor_to_payload(self.snapshot)
                if self.snapshot is not None
                else None
            ),
            "committed_count": self.committed_count,
            "records": [record.to_dict() for record in self.records],
            "budget_waits": self.budget_waits,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "RoundState":
        snapshot = (
            tensor_from_payload(payload["snapshot"])
            if payload.get("snapshot") is not None
            else None
        )
        state = cls(
            round_index=int(payload["round_index"]),
            snapshot_hash=str(payload["snapshot_hash"]),
            parameter_version=int(payload["parameter_version"]),
            specs=[BlockSpec.from_dict(item) for item in payload["specs"]],
            snapshot=snapshot,
            committed_count=int(payload["committed_count"]),
            records=[BlockRecord.from_dict(item) for item in payload["records"]],
            budget_waits=int(payload.get("budget_waits", 0)),
        )
        if snapshot is not None and tensor_content_hash(snapshot) != state.snapshot_hash:
            fail(
                "E_CHECKPOINT",
                "stored block snapshot does not match its hash",
                "block_ledger.snapshot",
            )
        return state


class BlockLedger:
    """Append-only round ledger with strict ordered commit semantics."""

    def __init__(self) -> None:
        self.rounds: dict[int, RoundState] = {}
        self._lock = threading.RLock()

    def begin_round(
        self,
        round_index: int,
        specs: list[BlockSpec],
        snapshot_hash: str,
        parameter_version: int,
        snapshot: torch.Tensor | None = None,
    ) -> RoundState:
        if round_index in self.rounds:
            fail(
                "E_STATE",
                "round already exists in block ledger",
                "round_index",
                value=round_index,
            )
        expected = list(range(len(specs)))
        if [spec.sequence for spec in specs] != expected:
            fail(
                "E_STATE",
                "block specifications must have contiguous sequence numbers",
                "blocks.sequence",
            )
        state = RoundState(
            round_index=round_index,
            snapshot_hash=snapshot_hash,
            parameter_version=parameter_version,
            specs=list(specs),
            snapshot=snapshot.detach().clone() if snapshot is not None else None,
        )
        self.rounds[round_index] = state
        return state

    def _round(self, round_index: int) -> RoundState:
        if round_index not in self.rounds:
            fail("E_STATE", "block round does not exist", "round_index", value=round_index)
        return self.rounds[round_index]

    def submit(
        self,
        record: BlockRecord,
        grad_buffer: torch.Tensor,
        *,
        expected_sequence: int | None = None,
    ) -> None:
        state = self._round(record.round_index)
        expected = (
            state.committed_count if expected_sequence is None else expected_sequence
        )
        if record.status != "committed":
            fail("E_STATE", "only committed records may be submitted", "status")
        if record.sequence != expected or record.sequence != state.committed_count:
            fail(
                "E_STATE",
                "out-of-order block commit rejected",
                "blocks.sequence",
                expected=state.committed_count,
                actual=record.sequence,
            )
        if record.sequence >= len(state.specs):
            fail("E_STATE", "block sequence exceeds round block count", "blocks.sequence")
        spec = state.specs[record.sequence]
        if (
            record.block_id != spec.block_id
            or record.shadow_seed != spec.shadow_seed
        ):
            fail("E_STATE", "block identity does not match the round plan", "block_id")
        if record.snapshot_hash != state.snapshot_hash:
            fail(
                "E_STATE",
                "block snapshot changed after round start; committed blocks are locked",
                "snapshot_hash",
            )
        if record.parameter_version != state.parameter_version:
            fail(
                "E_STATE",
                "block parameter version changed after round start",
                "parameter_version",
            )
        duplicate = any(
            existing.status == "committed" and existing.block_id == record.block_id
            for existing in state.records
        )
        if duplicate:
            fail("E_STATE", "block already committed", "block_id", value=record.block_id)
        record.grad_buffer = grad_buffer.detach().clone()
        state.records.append(record)
        state.committed_count += 1

    def record_failure(self, record: BlockRecord) -> None:
        with self._lock:
            state = self._round(record.round_index)
            if record.status != "failed":
                fail("E_STATE", "failure record must have failed status", "status")
            if record.sequence < state.committed_count:
                fail(
                    "E_STATE",
                    "failure record refers to an already committed block",
                    "blocks.sequence",
                )
            if record.snapshot_hash != state.snapshot_hash:
                fail("E_STATE", "failure record has a drifted snapshot", "snapshot_hash")
            state.records.append(record)

    def note_budget_wait(self, round_index: int) -> None:
        with self._lock:
            self._round(round_index).budget_waits += 1

    def committed_sequence(self) -> list[str]:
        sequence: list[str] = []
        for round_index in sorted(self.rounds):
            sequence.extend(
                f"r{round_index}:{block_id}"
                for block_id in self.rounds[round_index].committed_ids
            )
        return sequence

    def latest_round(self) -> RoundState | None:
        if not self.rounds:
            return None
        return self.rounds[max(self.rounds)]

    def to_dict(self) -> dict[str, Any]:
        return {
            "rounds": [
                self.rounds[index].to_dict() for index in sorted(self.rounds)
            ]
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "BlockLedger":
        ledger = cls()
        for item in payload.get("rounds", []):
            state = RoundState.from_dict(item)
            if state.round_index in ledger.rounds:
                fail("E_CHECKPOINT", "duplicate round in block ledger", "rounds")
            ledger.rounds[state.round_index] = state
        return ledger


@dataclass
class RoundExecutionResult:
    loss: float
    gradient: torch.Tensor
    records: list[BlockRecord]
    budget_waits: int
    retries: int


BlockFunction = Callable[[BlockSpec, torch.Tensor], torch.Tensor]
FaultFunction = Callable[[str, int, BlockSpec, int], None]


def _as_business_error(exc: BaseException) -> AppError:
    if isinstance(exc, AppError):
        return exc
    return AppError("E_BLOCK", f"block execution failed: {exc}")


def execute_block_round(
    *,
    ledger: BlockLedger,
    round_index: int,
    parameter_version: int,
    snapshot: torch.Tensor,
    specs: list[BlockSpec],
    block_function: BlockFunction,
    buffer_cost: Callable[[BlockSpec], int],
    buffer_limit_elements: int,
    max_parallel_blocks: int,
    max_retries: int = 1,
    fault_function: FaultFunction | None = None,
) -> RoundExecutionResult:
    if max_parallel_blocks <= 0:
        fail("E_PARA", "max_parallel_blocks must be positive", "max_parallel_blocks")
    if buffer_limit_elements <= 0:
        fail("E_PARA", "buffer_limit_elements must be positive", "buffer_limit_elements")
    if max_retries < 0:
        fail("E_PARA", "max_retries must be non-negative", "max_retries")
    snapshot = snapshot.detach().clone()
    snapshot_hash = tensor_content_hash(snapshot)
    state = ledger.begin_round(
        round_index,
        specs,
        snapshot_hash,
        parameter_version,
        snapshot=snapshot,
    )
    gradient_accumulator = torch.zeros_like(snapshot)
    loss_accumulator = 0.0
    retry_count = 0

    def run_one(spec: BlockSpec) -> tuple[BlockRecord, torch.Tensor | None]:
        started = time.perf_counter()
        attempts = 0
        last_error: AppError | None = None
        while attempts <= max_retries:
            attempts += 1
            local_snapshot = snapshot.detach().clone().requires_grad_(True)
            try:
                if fault_function is not None:
                    fault_function("forward", round_index, spec, attempts)
                block_loss = block_function(spec, local_snapshot)
                if not block_loss.requires_grad:
                    fail(
                        "E_BLOCK",
                        "block loss is detached from the parameter snapshot",
                        "loss",
                    )
                if fault_function is not None:
                    fault_function("backward", round_index, spec, attempts)
                local_gradient = torch.autograd.grad(block_loss, local_snapshot)[0]
                if not torch.isfinite(local_gradient).all():
                    fail(
                        "E_BLOCK",
                        "block backpropagation produced non-finite gradients",
                        "gradient",
                    )
                duration_ms = (time.perf_counter() - started) * 1000.0
                record = BlockRecord(
                    round_index=round_index,
                    sequence=spec.sequence,
                    block_id=spec.block_id,
                    status="committed",
                    snapshot_hash=snapshot_hash,
                    parameter_version=parameter_version,
                    shadow_seed=spec.shadow_seed,
                    attempts=attempts,
                    loss=float(block_loss.detach().item()),
                    gradient_norm=float(local_gradient.norm().item()),
                    duration_ms=duration_ms,
                    buffer_elements=buffer_cost(spec),
                )
                return record, local_gradient
            except BaseException as exc:  # noqa: BLE001 - converted to structured errors
                last_error = _as_business_error(exc)
                duration_ms = (time.perf_counter() - started) * 1000.0
                ledger.record_failure(
                    BlockRecord(
                        round_index=round_index,
                        sequence=spec.sequence,
                        block_id=spec.block_id,
                        status="failed",
                        snapshot_hash=snapshot_hash,
                        parameter_version=parameter_version,
                        shadow_seed=spec.shadow_seed,
                        attempts=attempts,
                        loss=None,
                        gradient_norm=None,
                        duration_ms=duration_ms,
                        error_code=last_error.code,
                        error_message=last_error.message,
                        buffer_elements=buffer_cost(spec),
                    )
                )
                if attempts > max_retries:
                    break
        assert last_error is not None
        raise last_error

    pending: list[tuple[BlockSpec, int, Future[tuple[BlockRecord, torch.Tensor | None]]]] = []
    next_index = 0
    used_elements = 0
    budget_waits = 0

    with ThreadPoolExecutor(max_workers=max_parallel_blocks) as executor:
        while next_index < len(specs) or pending:
            launched = False
            while next_index < len(specs) and len(pending) < max_parallel_blocks:
                spec = specs[next_index]
                cost = buffer_cost(spec)
                if cost > buffer_limit_elements:
                    fail(
                        "E_PARA",
                        "a single block exceeds the buffer budget",
                        "buffer_limit_elements",
                        block_id=spec.block_id,
                        block_elements=cost,
                        buffer_limit_elements=buffer_limit_elements,
                    )
                if used_elements + cost > buffer_limit_elements:
                    budget_waits += 1
                    ledger.note_budget_wait(round_index)
                    break
                used_elements += cost
                pending.append((spec, cost, executor.submit(run_one, spec)))
                next_index += 1
                launched = True
            if not pending:
                if not launched:
                    fail(
                        "E_PARA",
                        "buffer budget cannot schedule the next block",
                        "buffer_limit_elements",
                    )
                continue
            spec, cost, future = pending.pop(0)
            record, local_gradient = future.result()
            if local_gradient is None:
                fail("E_BLOCK", "block completed without a gradient", "gradient")
            ledger.submit(record, local_gradient)
            gradient_accumulator.add_(local_gradient)
            loss_accumulator += float(record.loss or 0.0)
            used_elements -= cost
            if record.attempts > 1:
                retry_count += record.attempts - 1

    records = list(state.records)
    return RoundExecutionResult(
        loss=loss_accumulator,
        gradient=gradient_accumulator,
        records=records,
        budget_waits=budget_waits,
        retries=retry_count,
    )


def make_full_image_specs(
    targets: list[tuple[int, str, str, int, int]],
    *,
    base_seed: int,
) -> list[BlockSpec]:
    specs: list[BlockSpec] = []
    for target_index, target_id, camera_id, width, height in targets:
        specs.append(
            BlockSpec(
                sequence=len(specs),
                block_id=f"i{target_index:03d}:full",
                image_index=target_index,
                target_id=target_id,
                camera_id=camera_id,
                x_offset=0,
                y_offset=0,
                width=width,
                height=height,
                shadow_seed=base_seed + target_index,
            )
        )
    return specs


def make_tile_specs(
    targets: list[tuple[int, str, str, int, int]],
    *,
    tile_size: int,
    base_seed: int,
) -> list[BlockSpec]:
    if tile_size <= 0:
        fail("E_PARA", "tile_size must be positive", "tile_size")
    specs: list[BlockSpec] = []
    for target_index, target_id, camera_id, width, height in targets:
        for y_offset in range(0, height, tile_size):
            tile_height = min(tile_size, height - y_offset)
            for x_offset in range(0, width, tile_size):
                tile_width = min(tile_size, width - x_offset)
                specs.append(
                    BlockSpec(
                        sequence=len(specs),
                        block_id=(
                            f"i{target_index:03d}:y{y_offset:04d}:x{x_offset:04d}"
                        ),
                        image_index=target_index,
                        target_id=target_id,
                        camera_id=camera_id,
                        x_offset=x_offset,
                        y_offset=y_offset,
                        width=tile_width,
                        height=tile_height,
                        shadow_seed=base_seed + target_index,
                    )
                )
    return specs

