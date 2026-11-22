"""Deterministic inverse-rendering optimization across execution strategies."""

from __future__ import annotations

import math
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import numpy as np
import torch

from .blocks import (
    BlockLedger,
    BlockSpec,
    FaultFunction,
    execute_block_round,
    make_full_image_specs,
    make_tile_specs,
)
from .checkpoint import ALGORITHM_VERSION, CheckpointStore
from .errors import AppError, fail
from .gradient import GradientGuard, GuardConfig
from .params import (
    SceneParameterLayout,
    build_layout,
    physical_values_from_raw,
    raw_from_scene,
    scene_from_raw,
    truth_values,
)
from .renderer import RenderConfig, render_image, render_tile
from .schema import Task, decode_target
from .serialization import canonical_hash, tensor_from_payload, tensor_to_payload
from .strategy import (
    ScaleConfig,
    StrategyDecision,
    choose_strategy,
    estimated_element_cost,
)


@dataclass(frozen=True)
class RunConfig:
    iterations: int = 20
    learning_rate: float = 0.012
    learning_rate_min: float = 2.0e-4
    lr_schedule: str = "cosine"
    mode: str = "auto"
    memory_budget_mb: float = 64.0
    max_exact_pixels: int = 262_144
    max_exact_iterations: int = 200
    downsample_iteration_threshold: int = 60
    tile_size: int = 16
    max_parallel_blocks: int = 2
    requested_downsample_factor: int = 0
    clip_enabled: bool = True
    clip_norm: float = 5.0
    explode_threshold: float = 1.0e4
    failure_patience: int = 2
    checkpoint_every: int = 2
    max_block_retries: int = 1
    seed: int | None = None

    def validate(self) -> None:
        if isinstance(self.iterations, bool) or not isinstance(self.iterations, int):
            fail("E_PARA", "iterations must be an integer", "iterations")
        if not (1 <= self.iterations <= 100_000):
            fail("E_PARA", "iterations must be in [1, 100000]", "iterations")
        learning_rate = float(self.learning_rate)
        if not math.isfinite(learning_rate) or learning_rate <= 0.0:
            fail(
                "E_PARA",
                "learning_rate must be a positive finite number",
                "learning_rate",
            )
        learning_rate_min = float(self.learning_rate_min)
        if (
            not math.isfinite(learning_rate_min)
            or learning_rate_min < 0.0
            or learning_rate_min > learning_rate
        ):
            fail(
                "E_PARA",
                "learning_rate_min must be finite and in [0, learning_rate]",
                "learning_rate_min",
            )
        if self.lr_schedule not in {"none", "cosine"}:
            fail(
                "E_PARA",
                "lr_schedule must be none or cosine",
                "lr_schedule",
            )
        if isinstance(self.checkpoint_every, bool) or not isinstance(
            self.checkpoint_every, int
        ):
            fail("E_PARA", "checkpoint_every must be an integer", "checkpoint_every")
        if self.checkpoint_every < 0:
            fail(
                "E_PARA",
                "checkpoint_every must be non-negative",
                "checkpoint_every",
            )
        if isinstance(self.max_block_retries, bool) or not isinstance(
            self.max_block_retries, int
        ):
            fail(
                "E_PARA",
                "max_block_retries must be an integer",
                "max_block_retries",
            )
        if self.max_block_retries < 0:
            fail(
                "E_PARA",
                "max_block_retries must be non-negative",
                "max_block_retries",
            )
        if self.seed is not None and (
            isinstance(self.seed, bool) or not isinstance(self.seed, int)
        ):
            fail("E_PARA", "seed must be an integer or null", "seed")

    def scale_config(self, shadow_samples: int) -> ScaleConfig:
        return ScaleConfig(
            mode=self.mode,
            memory_budget_mb=self.memory_budget_mb,
            max_exact_pixels=self.max_exact_pixels,
            max_exact_iterations=self.max_exact_iterations,
            downsample_iteration_threshold=self.downsample_iteration_threshold,
            tile_size=self.tile_size,
            max_parallel_blocks=self.max_parallel_blocks,
            requested_downsample_factor=self.requested_downsample_factor,
            shadow_samples=shadow_samples,
        )

    def guard_config(self) -> GuardConfig:
        return GuardConfig(
            clip_enabled=self.clip_enabled,
            clip_norm=self.clip_norm,
            explode_threshold=self.explode_threshold,
            failure_patience=self.failure_patience,
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def fingerprint_payload(self, resolved_seed: int) -> dict[str, Any]:
        return {
            "iterations": self.iterations,
            "learning_rate": self.learning_rate,
            "learning_rate_min": self.learning_rate_min,
            "lr_schedule": self.lr_schedule,
            "mode": self.mode,
            "memory_budget_mb": self.memory_budget_mb,
            "max_exact_pixels": self.max_exact_pixels,
            "max_exact_iterations": self.max_exact_iterations,
            "downsample_iteration_threshold": self.downsample_iteration_threshold,
            "tile_size": self.tile_size,
            "max_parallel_blocks": self.max_parallel_blocks,
            "requested_downsample_factor": self.requested_downsample_factor,
            "seed": resolved_seed,
            "guard": self.guard_config().fingerprint_payload(),
            "max_block_retries": self.max_block_retries,
        }


@dataclass
class CheckpointSummary:
    path: str
    content_hash: str
    parameter_version: int
    iteration: int
    bytes: int


@dataclass
class OptimizationReport:
    task_id: str
    strategy: dict[str, Any]
    config: dict[str, Any]
    final_parameters: dict[str, float]
    truth_parameters: dict[str, float]
    loss_history: list[float]
    gradient_history: list[dict[str, Any]]
    gradient_clip_events: int
    submission_sequence: list[str]
    block_records: list[dict[str, Any]]
    block_statistics: dict[str, Any]
    final_loss: float
    iterations_completed: int
    parameter_version: int
    checkpoints: list[dict[str, Any]]
    checkpoint_load: dict[str, Any]
    parameter_errors: dict[str, dict[str, float]]

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


class OptimizationRunner:
    def __init__(
        self,
        task: Task,
        config: RunConfig,
        *,
        checkpoint_dir: str | os.PathLike[str] | None = None,
        gradient_spikes: dict[int, dict[str, float]] | None = None,
        fault_function: FaultFunction | None = None,
    ) -> None:
        config.validate()
        self.task = task
        self.config = config
        self.resolved_seed = (
            task.scene.target_seed if config.seed is None else int(config.seed)
        )
        self.layout = build_layout(task.scene)
        self.strategy = choose_strategy(
            object_count=len(task.scene.geometries),
            width=task.targets[0].width,
            height=task.targets[0].height,
            image_count=len(task.targets),
            iterations=config.iterations,
            config=config.scale_config(task.scene.soft_shadow_samples),
        )
        self.camera_by_id = {camera.id: camera for camera in task.scene.cameras}
        self.target_by_id = {target.id: target for target in task.targets}
        self.target_tensors = self._load_targets()
        self.total_elements = sum(
            tensor.numel() for tensor in self.target_tensors
        )
        self.config_fingerprint = canonical_hash(
            {
                "algorithm_version": ALGORITHM_VERSION,
                "run": config.fingerprint_payload(self.resolved_seed),
                "scene_id": task.scene.scene_id,
                "layout_labels": list(self.layout.labels),
            }
        )
        self.gradient_spikes = gradient_spikes or {}
        self.fault_function = fault_function
        self.checkpoint_store = (
            CheckpointStore(checkpoint_dir) if checkpoint_dir is not None else None
        )
        self.checkpoint_load: dict[str, Any] = {
            "valid": False,
            "reason": "not_attempted",
        }
        self.checkpoints: list[CheckpointSummary] = []
        self._initialize_fresh()

    def _load_targets(self) -> list[torch.Tensor]:
        tensors: list[torch.Tensor] = []
        for target in self.task.targets:
            array = decode_target(self.task.dataset_root / target.path, target)
            tensors.append(torch.tensor(array, dtype=torch.float64))
        return tensors

    def _initialize_fresh(self) -> None:
        self.raw = torch.nn.Parameter(
            raw_from_scene(self.task.scene, self.layout).requires_grad_(True)
        )
        self.optimizer = torch.optim.Adam(
            [self.raw], lr=float(self.config.learning_rate)
        )
        self.guard = GradientGuard(self.layout, self.config.guard_config())
        self.ledger = BlockLedger()
        self.loss_history: list[float] = []
        self.iterations_completed = 0
        self.parameter_version = 0
        self.last_loss = float("nan")

    def load_checkpoint(self) -> dict[str, Any]:
        if self.checkpoint_store is None:
            fail(
                "E_STATE",
                "checkpoint directory is required for resume",
                "checkpoint_dir",
            )
        result = self.checkpoint_store.load(
            expected_config_fingerprint=self.config_fingerprint,
            expected_dataset_fingerprint=self.task.source_fingerprint,
        )
        self.checkpoint_load = result.to_dict()
        if not result.valid or result.payload is None:
            self._initialize_fresh()
            self.checkpoint_load["action"] = "full_recompute"
            return self.checkpoint_load
        try:
            self._restore_payload(result.payload)
            self.checkpoint_load["action"] = "resumed"
        except Exception as exc:  # noqa: BLE001 - any invalid snapshot is discarded
            self._initialize_fresh()
            self.checkpoint_load = {
                **result.to_dict(),
                "valid": False,
                "reason": f"snapshot_restore_failed:{exc}",
                "requires_full_recompute": True,
                "action": "full_recompute",
            }
        return self.checkpoint_load

    def _restore_payload(self, payload: dict[str, Any]) -> None:
        if payload.get("iterations_completed") != payload.get("parameter_version"):
            fail(
                "E_CHECKPOINT",
                "iteration and parameter version disagree",
                "parameter_version",
            )
        incoming_strategy = payload.get("strategy", {})
        if incoming_strategy.get("mode") != self.strategy.mode:
            fail(
                "E_CHECKPOINT",
                "strategy in checkpoint differs from current selection",
                "strategy.mode",
            )
        raw_payload = payload.get("raw_parameters")
        if not isinstance(raw_payload, dict):
            fail("E_CHECKPOINT", "raw parameter snapshot is missing", "raw_parameters")
        raw_tensor = tensor_from_payload(raw_payload, requires_grad=True)
        if raw_tensor.shape != (self.layout.size,):
            fail(
                "E_CHECKPOINT",
                "raw parameter snapshot has the wrong shape",
                "raw_parameters",
            )
        if not torch.isfinite(raw_tensor).all():
            fail(
                "E_CHECKPOINT",
                "raw parameter snapshot contains non-finite values",
                "raw_parameters",
            )
        iteration = int(payload["iterations_completed"])
        if not (0 <= iteration <= self.config.iterations):
            fail(
                "E_CHECKPOINT",
                "checkpoint iteration is outside the configured run",
                "iterations_completed",
            )
        loss_history = [float(v) for v in payload.get("loss_history", [])]
        if len(loss_history) != iteration:
            fail(
                "E_CHECKPOINT",
                "loss history length does not match checkpoint iteration",
                "loss_history",
            )
        if not all(math.isfinite(v) for v in loss_history):
            fail("E_CHECKPOINT", "loss history contains non-finite values", "loss_history")
        self.raw = torch.nn.Parameter(raw_tensor)
        self.optimizer = torch.optim.Adam(
            [self.raw], lr=float(self.config.learning_rate)
        )
        optimizer_payload = payload.get("optimizer")
        if not isinstance(optimizer_payload, dict):
            fail("E_CHECKPOINT", "optimizer state is missing", "optimizer")
        state = self.optimizer.state[self.raw]
        for name in ("step", "exp_avg", "exp_avg_sq"):
            if name not in optimizer_payload:
                fail(
                    "E_CHECKPOINT",
                    f"optimizer state is missing {name}",
                    f"optimizer.{name}",
                )
            state[name] = tensor_from_payload(optimizer_payload[name])
        self.optimizer.state[self.raw] = state
        guard_payload = payload.get("guard")
        if not isinstance(guard_payload, dict):
            fail("E_CHECKPOINT", "gradient guard state is missing", "guard")
        self.guard = GradientGuard.from_dict(
            self.layout, self.config.guard_config(), guard_payload
        )
        ledger_payload = payload.get("block_ledger")
        if not isinstance(ledger_payload, dict):
            fail("E_CHECKPOINT", "block ledger is missing", "block_ledger")
        self.ledger = BlockLedger.from_dict(ledger_payload)
        if len(self.guard.history) != iteration:
            fail(
                "E_CHECKPOINT",
                "gradient history length does not match checkpoint iteration",
                "guard.history",
            )
        if len(self.ledger.rounds) != iteration:
            fail(
                "E_CHECKPOINT",
                "block ledger round count does not match checkpoint iteration",
                "block_ledger.rounds",
            )
        self.loss_history = loss_history
        self.iterations_completed = iteration
        self.parameter_version = iteration
        self.last_loss = loss_history[-1] if loss_history else float("nan")

    def _render_config(self, downsample_factor: int = 1) -> RenderConfig:
        return RenderConfig(
            shadow_samples=self.task.scene.soft_shadow_samples,
            ray_epsilon=self.task.scene.ray_epsilon,
            downsample_factor=downsample_factor,
            base_seed=self.resolved_seed,
        )

    def _specs(self) -> list[BlockSpec]:
        target_entries = [
            (index, target.id, target.camera_id, target.width, target.height)
            for index, target in enumerate(self.task.targets)
        ]
        if self.strategy.mode == "tiled":
            return make_tile_specs(
                target_entries,
                tile_size=self.strategy.tile_size,
                base_seed=self.resolved_seed,
            )
        return make_full_image_specs(target_entries, base_seed=self.resolved_seed)

    def _block_function(self, spec: BlockSpec, snapshot: torch.Tensor) -> torch.Tensor:
        scene = scene_from_raw(self.task.scene, self.layout, snapshot)
        target = self.task.targets[spec.image_index]
        target_array = self.target_tensors[spec.image_index]
        camera = self.camera_by_id[target.camera_id]
        if self.strategy.mode == "tiled":
            image = render_tile(
                scene,
                camera,
                target,
                x_offset=spec.x_offset,
                y_offset=spec.y_offset,
                tile_width=spec.width,
                tile_height=spec.height,
                shadow_samples=self.task.scene.soft_shadow_samples,
                seed=spec.shadow_seed,
            )
            target_slice = target_array[
                spec.y_offset : spec.y_offset + spec.height,
                spec.x_offset : spec.x_offset + spec.width,
            ]
        else:
            factor = (
                self.strategy.downsample_factor
                if self.strategy.mode == "downsample"
                else 1
            )
            image = render_image(
                scene,
                camera,
                target,
                self._render_config(factor),
                seed_offset=spec.image_index,
            )
            target_slice = target_array
        difference = image - target_slice
        return difference.square().sum() / float(self.total_elements)

    def _buffer_cost(self, spec: BlockSpec) -> int:
        if self.strategy.mode == "downsample":
            factor = self.strategy.downsample_factor
            pixels = max(1, math.ceil(spec.width / factor)) * max(
                1, math.ceil(spec.height / factor)
            )
        else:
            pixels = spec.pixels
        return estimated_element_cost(
            object_count=len(self.task.scene.geometries),
            shadow_samples=self.task.scene.soft_shadow_samples,
            pixels=pixels,
        )

    def _run_iteration(self, iteration: int) -> None:
        specs = self._specs()
        result = execute_block_round(
            ledger=self.ledger,
            round_index=iteration,
            parameter_version=self.parameter_version,
            snapshot=self.raw.detach(),
            specs=specs,
            block_function=self._block_function,
            buffer_cost=self._buffer_cost,
            buffer_limit_elements=self.strategy.buffer_limit_elements,
            max_parallel_blocks=self.strategy.max_parallel_blocks,
            max_retries=self.config.max_block_retries,
            fault_function=self.fault_function,
        )
        self.raw.grad = result.gradient
        guard_report = self.guard.process(
            self.raw,
            iteration=iteration,
            gradient_spikes=self.gradient_spikes.get(iteration),
        )
        if self.config.lr_schedule == "cosine":
            denominator = max(1, self.config.iterations - 1)
            progress = min(1.0, float(iteration) / float(denominator))
            scheduled = self.config.learning_rate_min + (
                self.config.learning_rate - self.config.learning_rate_min
            ) * 0.5 * (1.0 + math.cos(math.pi * progress))
            for group in self.optimizer.param_groups:
                group["lr"] = scheduled
        before_step = self.raw.detach().clone()
        self.optimizer.step()
        protected = set(self.guard.frozen) | set(guard_report.suspect_parameters)
        for label in protected:
            index = self.layout.label_to_index[label]
            self.raw.data[index] = before_step[index]
        self.raw.grad = None
        self.loss_history.append(float(result.loss))
        self.last_loss = float(result.loss)
        self.iterations_completed += 1
        self.parameter_version += 1
        self.guard.raise_if_failed(guard_report)

    def _current_loss(self) -> float:
        scene = scene_from_raw(self.task.scene, self.layout, self.raw.detach())
        total = 0.0
        for index, target in enumerate(self.task.targets):
            camera = self.camera_by_id[target.camera_id]
            factor = (
                self.strategy.downsample_factor
                if self.strategy.mode == "downsample"
                else 1
            )
            with torch.no_grad():
                image = render_image(
                    scene,
                    camera,
                    target,
                    self._render_config(factor),
                    seed_offset=index,
                )
            total += float(
                (image - self.target_tensors[index]).square().sum().item()
                / float(self.total_elements)
            )
        return total

    def render_final_images(self) -> list[torch.Tensor]:
        scene = scene_from_raw(self.task.scene, self.layout, self.raw.detach())
        images: list[torch.Tensor] = []
        for index, target in enumerate(self.task.targets):
            camera = self.camera_by_id[target.camera_id]
            factor = (
                self.strategy.downsample_factor
                if self.strategy.mode == "downsample"
                else 1
            )
            with torch.no_grad():
                image = render_image(
                    scene,
                    camera,
                    target,
                    self._render_config(factor),
                    seed_offset=index,
                )
            images.append(image)
        return images

    def _optimizer_payload(self) -> dict[str, Any]:
        state = self.optimizer.state.get(self.raw)
        if not state:
            fail(
                "E_CHECKPOINT",
                "optimizer has no state to checkpoint",
                "optimizer",
            )
        return {
            name: tensor_to_payload(state[name])
            for name in ("step", "exp_avg", "exp_avg_sq")
        }

    def _validate_checkpoint_state(self) -> None:
        if not torch.isfinite(self.raw).all():
            fail("E_CHECKPOINT", "parameters are non-finite at checkpoint", "parameters")
        if not all(math.isfinite(loss) for loss in self.loss_history):
            fail("E_CHECKPOINT", "loss history is non-finite", "loss_history")
        if len(self.loss_history) != self.iterations_completed:
            fail(
                "E_CHECKPOINT",
                "loss history length does not match completed iterations",
                "loss_history",
            )
        latest = self.ledger.latest_round()
        if self.iterations_completed > 0 and latest is None:
            fail("E_CHECKPOINT", "block ledger has no completed round", "block_ledger")
        if latest is not None and latest.committed_count != len(latest.specs):
            fail(
                "E_CHECKPOINT",
                "latest block round is incomplete",
                "block_ledger.committed_count",
            )

    def save_checkpoint(self) -> dict[str, Any] | None:
        if self.checkpoint_store is None:
            return None
        self._validate_checkpoint_state()
        payload = {
            "task_id": self.task.scene.scene_id,
            "group_id": self.task.group_id,
            "iterations_completed": self.iterations_completed,
            "raw_parameters": tensor_to_payload(self.raw),
            "optimizer": self._optimizer_payload(),
            "guard": self.guard.to_dict(),
            "block_ledger": self.ledger.to_dict(),
            "loss_history": list(self.loss_history),
            "strategy": self.strategy.to_dict(),
            "config": self.config.to_dict(),
        }
        saved = self.checkpoint_store.save(
            payload,
            parameter_version=self.parameter_version,
            config_fingerprint=self.config_fingerprint,
            dataset_fingerprint=self.task.source_fingerprint,
        )
        summary = CheckpointSummary(
            path=saved["path"],
            content_hash=saved["content_hash"],
            parameter_version=saved["parameter_version"],
            iteration=self.iterations_completed,
            bytes=saved["bytes"],
        )
        self.checkpoints.append(summary)
        return asdict(summary)

    def run_to(self, target_iterations: int) -> None:
        if isinstance(target_iterations, bool) or not isinstance(target_iterations, int):
            fail("E_PARA", "target_iterations must be an integer", "target_iterations")
        if target_iterations < self.iterations_completed:
            fail(
                "E_STATE",
                "requested target iteration precedes current state",
                "target_iterations",
                current=self.iterations_completed,
                requested=target_iterations,
            )
        if target_iterations > self.config.iterations:
            fail(
                "E_PARA",
                "target_iterations exceeds configured iterations",
                "target_iterations",
                configured=self.config.iterations,
            )
        while self.iterations_completed < target_iterations:
            iteration = self.iterations_completed
            self._run_iteration(iteration)
            completed = self.iterations_completed
            window_due = (
                self.config.checkpoint_every > 0
                and completed % self.config.checkpoint_every == 0
            )
            if self.checkpoint_store is not None and (
                window_due or completed == target_iterations
            ):
                self.save_checkpoint()

    def block_statistics(self) -> dict[str, Any]:
        records: list[Any] = []
        for round_index in sorted(self.ledger.rounds):
            records.extend(self.ledger.rounds[round_index].records)
        committed = [record for record in records if record.status == "committed"]
        failed = [record for record in records if record.status == "failed"]
        durations = [record.duration_ms for record in committed]
        per_block: dict[str, dict[str, float]] = {}
        for record in committed:
            entry = per_block.setdefault(
                record.block_id,
                {"count": 0.0, "total_ms": 0.0, "max_ms": 0.0},
            )
            entry["count"] += 1.0
            entry["total_ms"] += record.duration_ms
            entry["max_ms"] = max(entry["max_ms"], record.duration_ms)
        return {
            "committed_blocks": len(committed),
            "failed_attempts": len(failed),
            "retries": sum(max(0, record.attempts - 1) for record in committed),
            "total_ms": sum(durations),
            "mean_ms": (sum(durations) / len(durations)) if durations else 0.0,
            "max_ms": max(durations) if durations else 0.0,
            "per_block": per_block,
        }

    def parameter_errors(self) -> dict[str, dict[str, float]]:
        final = physical_values_from_raw(self.task.scene, self.layout, self.raw.detach())
        truth = truth_values(
            self.task.scene, self.layout, self.task.truth.values
        )
        errors: dict[str, dict[str, float]] = {}
        for label in self.layout.labels:
            absolute = abs(final[label] - truth[label])
            relative = absolute / max(abs(truth[label]), 1.0e-12)
            errors[label] = {
                "estimate": final[label],
                "truth": truth[label],
                "absolute_error": absolute,
                "relative_error": relative,
            }
        return errors

    def report(self) -> OptimizationReport:
        final_parameters = physical_values_from_raw(
            self.task.scene, self.layout, self.raw.detach()
        )
        truth = truth_values(self.task.scene, self.layout, self.task.truth.values)
        block_records = []
        for round_index in sorted(self.ledger.rounds):
            for record in self.ledger.rounds[round_index].records:
                block_records.append(
                    {
                        "round_index": record.round_index,
                        "sequence": record.sequence,
                        "block_id": record.block_id,
                        "status": record.status,
                        "parameter_version": record.parameter_version,
                        "snapshot_hash": record.snapshot_hash,
                        "shadow_seed": record.shadow_seed,
                        "attempts": record.attempts,
                        "loss": record.loss,
                        "gradient_norm": record.gradient_norm,
                        "duration_ms": record.duration_ms,
                        "error_code": record.error_code,
                        "error_message": record.error_message,
                    }
                )
        return OptimizationReport(
            task_id=self.task.scene.scene_id,
            strategy=self.strategy.to_dict(),
            config={
                **self.config.to_dict(),
                "resolved_seed": self.resolved_seed,
                "config_fingerprint": self.config_fingerprint,
                "dataset_fingerprint": self.task.source_fingerprint,
            },
            final_parameters=final_parameters,
            truth_parameters=truth,
            loss_history=list(self.loss_history),
            gradient_history=list(self.guard.history),
            gradient_clip_events=self.guard.clip_events_total,
            submission_sequence=self.ledger.committed_sequence(),
            block_records=block_records,
            block_statistics=self.block_statistics(),
            final_loss=self._current_loss(),
            iterations_completed=self.iterations_completed,
            parameter_version=self.parameter_version,
            checkpoints=[asdict(summary) for summary in self.checkpoints],
            checkpoint_load=dict(self.checkpoint_load),
            parameter_errors=self.parameter_errors(),
        )

