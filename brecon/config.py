from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .canonical import MODEL_VERSION, compact_json, read_json, sha256_hex
from .errors import BreconError


@dataclass(frozen=True)
class Observation:
    start_step: int
    end_step: int
    sync_steps: tuple[int, ...]
    tolerance: int


@dataclass(frozen=True)
class SampleConfig:
    raw: dict[str, Any]
    sample_dir: Path
    model_version: str
    room_id: str
    players: tuple[str, ...]
    step_ms: int
    origin_ts_ms: int
    compensation_window_ms: int
    observation: Observation
    boxes: tuple[dict[str, Any], ...]
    initial_inventory: dict[str, dict[str, int]]
    operations_file: str
    baseline_file: str
    trace_file: str
    latency_file: str
    snapshot_dir: str
    strict_latency: bool

    @property
    def digest(self) -> str:
        return sha256_hex(compact_json(self.raw))

    @property
    def operations_path(self) -> Path:
        return self.sample_dir / self.operations_file

    @property
    def baseline_path(self) -> Path:
        return self.sample_dir / self.baseline_file

    @property
    def trace_path(self) -> Path:
        return self.sample_dir / self.trace_file

    @property
    def latency_path(self) -> Path:
        return self.sample_dir / self.latency_file

    @property
    def snapshots_path(self) -> Path:
        return self.sample_dir / self.snapshot_dir

    @classmethod
    def load(cls, sample_dir: str | Path) -> "SampleConfig":
        sample_path = Path(sample_dir).resolve()
        config_path = sample_path / "config.json"
        if not config_path.is_file():
            raise BreconError(
                "E_CONFIG_INVALID",
                f"configuration not found: {config_path}",
                field_path="config.json",
            )
        try:
            raw = read_json(config_path)
        except (OSError, ValueError) as exc:
            raise BreconError(
                "E_CONFIG_INVALID",
                f"configuration is not valid JSON: {exc}",
                field_path="config.json",
            ) from exc

        required = (
            "model_version",
            "room_id",
            "players",
            "step_ms",
            "compensation_window_ms",
            "observation",
            "boxes",
            "initial_inventory",
        )
        for field in required:
            if field not in raw:
                raise BreconError(
                    "E_CONFIG_INVALID",
                    f"missing configuration field: {field}",
                    field_path=field,
                )

        model_version = str(raw["model_version"])
        if model_version != MODEL_VERSION:
            raise BreconError(
                "E_MODEL_VERSION",
                f"unsupported model version: {model_version}",
                field_path="config.model_version",
                details={"expected": MODEL_VERSION, "actual": model_version},
            )

        players = tuple(str(player) for player in raw["players"])
        if len(players) == 0 or len(set(players)) != len(players):
            raise BreconError(
                "E_CONFIG_INVALID",
                "players must be a non-empty unique list",
                field_path="config.players",
            )
        if int(raw["step_ms"]) <= 0:
            raise BreconError(
                "E_CONFIG_INVALID",
                "step_ms must be positive",
                field_path="config.step_ms",
            )

        observation_raw = raw["observation"]
        start_step = int(observation_raw["start_step"])
        end_step = int(observation_raw["end_step"])
        sync_steps = tuple(sorted({int(step) for step in observation_raw["sync_steps"]}))
        tolerance = int(observation_raw.get("tolerance", 0))
        if start_step < 1 or end_step < start_step:
            raise BreconError(
                "E_CONFIG_INVALID",
                "observation window is invalid",
                field_path="config.observation",
            )
        if any(step < start_step or step > end_step for step in sync_steps):
            raise BreconError(
                "E_CONFIG_INVALID",
                "sync step is outside the observation window",
                field_path="config.observation.sync_steps",
            )

        initial_inventory: dict[str, dict[str, int]] = {}
        for player in players:
            values = raw["initial_inventory"].get(player, {})
            initial_inventory[player] = {
                str(item): int(quantity) for item, quantity in values.items()
            }

        return cls(
            raw=raw,
            sample_dir=sample_path,
            model_version=model_version,
            room_id=str(raw["room_id"]),
            players=players,
            step_ms=int(raw["step_ms"]),
            origin_ts_ms=int(raw.get("origin_ts_ms", 0)),
            compensation_window_ms=int(raw["compensation_window_ms"]),
            observation=Observation(
                start_step=start_step,
                end_step=end_step,
                sync_steps=sync_steps,
                tolerance=tolerance,
            ),
            boxes=tuple(raw["boxes"]),
            initial_inventory=initial_inventory,
            operations_file=str(raw.get("operations_file", "operations.json")),
            baseline_file=str(raw.get("baseline_file", "baseline.json")),
            trace_file=str(raw.get("trace_file", "trace.jsonl")),
            latency_file=str(raw.get("latency_file", "latency.json")),
            snapshot_dir=str(raw.get("snapshot_dir", "snapshots")),
            strict_latency=bool(raw.get("strict_latency", True)),
        )

