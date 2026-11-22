"""Stable submission-order tile scheduler with explicit degradation trials."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any

from .config import DegradeStage, RenderConfig
from .errors import CancelledError
from .planning import PlanBudgetExceeded, TilePlan, plan_tile


@dataclass(frozen=True)
class Tile:
    sequence: int
    tile_id: str
    x: int
    y: int
    width: int
    height: int
    level: int = 0
    parent: str = "screen"

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_id": self.tile_id,
            "submit_seq": self.sequence,
            "x": self.x,
            "y": self.y,
            "width": self.width,
            "height": self.height,
            "level": self.level,
            "parent": self.parent,
        }


@dataclass
class StageTrial:
    stage: str
    step_multiplier: float
    shadow_samples: int
    admitted: bool
    measured_samples: int
    budget: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "step_multiplier": self.step_multiplier,
            "shadow_samples": self.shadow_samples,
            "admitted": self.admitted,
            "measured_samples": self.measured_samples,
            "budget": self.budget,
        }


@dataclass
class Admission:
    tile: Tile
    stage: DegradeStage
    plan: TilePlan
    sample_count: int
    trials: list[StageTrial]

    @property
    def degradation_index(self) -> int:
        return len(self.trials) - 1


@dataclass
class WaitingTask:
    tile: Tile
    trials: list[StageTrial]

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_id": self.tile.tile_id,
            "submit_seq": self.tile.sequence,
            "status": "WAITING",
            "budget": self.trials[-1].budget if self.trials else 0,
            "trials": [trial.to_dict() for trial in self.trials],
        }


@dataclass
class ScheduleResult:
    admitted: list[Admission]
    waiting: list[WaitingTask]
    decisions: list[dict[str, Any]]


def partition_screen(width: int, height: int, tile_size: int) -> list[Tile]:
    if tile_size <= 0:
        raise ValueError("tile_size must be positive")
    tiles: list[Tile] = []
    sequence = 0
    for y in range(0, height, tile_size):
        for x in range(0, width, tile_size):
            w = min(tile_size, width - x)
            h = min(tile_size, height - y)
            tile_id = f"t{sequence:05d}-{x}_{y}_{w}_{h}"
            tiles.append(Tile(sequence, tile_id, x, y, w, h))
            sequence += 1
    return tiles


def schedule_tiles(
    scene,
    tiles: list[Tile],
    config: RenderConfig,
    cancel_event: threading.Event | None = None,
) -> ScheduleResult:
    admitted: list[Admission] = []
    waiting: list[WaitingTask] = []
    decisions: list[dict[str, Any]] = []
    for tile in sorted(tiles, key=lambda item: item.sequence):
        trials: list[StageTrial] = []
        selected_stage: DegradeStage | None = None
        selected_plan: TilePlan | None = None
        selected_count = 0
        for stage in config.degrade_sequence:
            if cancel_event is not None and cancel_event.is_set():
                raise CancelledError()
            try:
                plan = plan_tile(
                    scene,
                    tile.x,
                    tile.y,
                    tile.width,
                    tile.height,
                    stage,
                    config,
                    budget=config.sample_budget,
                    cancel_event=cancel_event,
                )
            except PlanBudgetExceeded as exc:
                trials.append(
                    StageTrial(
                        stage=stage.name,
                        step_multiplier=stage.step_multiplier,
                        shadow_samples=stage.shadow_samples,
                        admitted=False,
                        measured_samples=exc.measured_samples,
                        budget=config.sample_budget,
                    )
                )
                continue
            trials.append(
                StageTrial(
                    stage=stage.name,
                    step_multiplier=stage.step_multiplier,
                    shadow_samples=stage.shadow_samples,
                    admitted=True,
                    measured_samples=plan.total_samples,
                    budget=config.sample_budget,
                )
            )
            selected_stage = stage
            selected_plan = plan
            selected_count = plan.total_samples
            break

        base_record: dict[str, Any] = {
            "event": "schedule",
            "tile_id": tile.tile_id,
            "submit_seq": tile.sequence,
            "bounds": [tile.x, tile.y, tile.width, tile.height],
            "budget": config.sample_budget,
            "parallelism": config.max_parallelism,
            "trials": [trial.to_dict() for trial in trials],
        }
        if selected_stage is None or selected_plan is None:
            waiting_task = WaitingTask(tile, trials)
            waiting.append(waiting_task)
            base_record.update(
                {
                    "status": "WAITING",
                    "reason": "all declared degradation stages exceed budget",
                }
            )
        else:
            admission = Admission(
                tile=tile,
                stage=selected_stage,
                plan=selected_plan,
                sample_count=selected_count,
                trials=trials,
            )
            admitted.append(admission)
            base_record.update(
                {
                    "status": "ADMITTED",
                    "stage": selected_stage.name,
                    "degradation_index": admission.degradation_index,
                    "step_multiplier": selected_stage.step_multiplier,
                    "shadow_samples": selected_stage.shadow_samples,
                    "sample_count": selected_count,
                    "budget_used_ratio": (
                        selected_count / config.sample_budget
                        if config.sample_budget
                        else 0.0
                    ),
                }
            )
        decisions.append(base_record)
    admitted.sort(key=lambda item: item.tile.sequence)
    waiting.sort(key=lambda item: item.tile.sequence)
    return ScheduleResult(admitted=admitted, waiting=waiting, decisions=decisions)
