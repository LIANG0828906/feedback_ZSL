"""Input models and fixed-point conversion helpers."""

from __future__ import annotations

import hashlib
import json
import math
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from typing import Any

from .errors import SimulationError

MICRO = 1_000_000
GENESIS_HASH = "0" * 64


def to_micro(value: float) -> int:
    """Convert a human-facing quantity to the deterministic integer domain."""

    number = float(value)
    if not math.isfinite(number):
        raise SimulationError("E_INPUT_INVALID", "non-finite numeric input", value=value)
    return round(number * MICRO)


def from_micro(value: int) -> float:
    return value / MICRO


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        separators=(",", ":"),
        allow_nan=False,
    )


def sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class CoverDefinition:
    cover_id: str
    x: float
    y: float
    width: float
    height: float
    hardness: float
    max_durability: int
    segment_threshold: int

    @property
    def center(self) -> tuple[float, float]:
        return (self.x + self.width / 2.0, self.y + self.height / 2.0)

    def contains(self, x: float, y: float) -> bool:
        return (
            self.x <= x <= self.x + self.width
            and self.y <= y <= self.y + self.height
        )

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> CoverDefinition:
        try:
            cover_id = str(data["cover_id"])
            hardness = float(data["hardness"])
            max_durability = to_micro(data["max_durability"])
            segment_threshold = to_micro(data["segment_threshold"])
            width = float(data["width"])
            height = float(data["height"])
            x = float(data["x"])
            y = float(data["y"])
        except (KeyError, TypeError, ValueError) as exc:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid cover definition", detail=str(exc)
            ) from exc
        if not cover_id or hardness <= 0 or max_durability <= 0 or segment_threshold <= 0:
            raise SimulationError(
                "E_INPUT_INVALID", "cover values must be positive", cover_id=cover_id
            )
        if width <= 0 or height <= 0:
            raise SimulationError(
                "E_INPUT_INVALID", "cover geometry must be positive", cover_id=cover_id
            )
        return cls(
            cover_id=cover_id,
            x=x,
            y=y,
            width=width,
            height=height,
            hardness=hardness,
            max_durability=max_durability,
            segment_threshold=segment_threshold,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "cover_id": self.cover_id,
            "x": self.x,
            "y": self.y,
            "width": self.width,
            "height": self.height,
            "hardness": self.hardness,
            "max_durability": from_micro(self.max_durability),
            "segment_threshold": from_micro(self.segment_threshold),
        }


@dataclass(frozen=True)
class InitialState:
    match_id: str
    step_ms: int
    start_step: int
    end_step: int
    compensation_window_steps: int
    spread_radius: float
    chain_decay: float
    max_chain_depth: int
    covers: Mapping[str, CoverDefinition]
    sample_rule: str
    truth_source: str
    initial_hash: str = field(init=False)

    def __post_init__(self) -> None:
        payload = self.content_dict()
        object.__setattr__(self, "initial_hash", sha256_text(canonical_json(payload)))

    @property
    def total_durability(self) -> int:
        return sum(cover.max_durability for cover in self.covers.values())

    @property
    def cover_ids(self) -> tuple[str, ...]:
        return tuple(sorted(self.covers))

    def content_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "match_id": self.match_id,
            "step_ms": self.step_ms,
            "observation": {
                "start_step": self.start_step,
                "end_step": self.end_step,
                "sample_rule": self.sample_rule,
            },
            "truth_source": self.truth_source,
            "compensation_window_steps": self.compensation_window_steps,
            "spread_radius": self.spread_radius,
            "chain_decay": self.chain_decay,
            "max_chain_depth": self.max_chain_depth,
            "covers": [
                self.covers[cover_id].to_dict() for cover_id in self.cover_ids
            ],
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> InitialState:
        try:
            observation = data["observation"]
            raw_covers = data["covers"]
            covers = {
                cover.cover_id: cover
                for cover in (CoverDefinition.from_dict(item) for item in raw_covers)
            }
            if len(covers) != len(raw_covers):
                raise ValueError("duplicate cover_id")
            state = cls(
                match_id=str(data["match_id"]),
                step_ms=int(data["step_ms"]),
                start_step=int(observation["start_step"]),
                end_step=int(observation["end_step"]),
                compensation_window_steps=int(data["compensation_window_steps"]),
                spread_radius=float(data["spread_radius"]),
                chain_decay=float(data["chain_decay"]),
                max_chain_depth=int(data["max_chain_depth"]),
                covers=covers,
                sample_rule=str(observation["sample_rule"]),
                truth_source=str(data["truth_source"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid initial state", detail=str(exc)
            ) from exc
        if state.step_ms <= 0:
            raise SimulationError("E_INPUT_INVALID", "step_ms must be positive")
        if state.start_step < 1 or state.end_step < state.start_step:
            raise SimulationError("E_INPUT_INVALID", "invalid observation window")
        if state.start_step != 1:
            raise SimulationError(
                "E_INPUT_INVALID",
                "the replay window must start at step 1 for complete arrival replay",
            )
        if state.compensation_window_steps < 0:
            raise SimulationError(
                "E_INPUT_INVALID", "compensation window cannot be negative"
            )
        if state.spread_radius <= 0 or not (0 < state.chain_decay <= 1):
            raise SimulationError("E_INPUT_INVALID", "invalid spread parameters")
        if state.max_chain_depth < 0:
            raise SimulationError("E_INPUT_INVALID", "invalid chain depth")
        return state

    @classmethod
    def load(cls, path: str) -> InitialState:
        with open(path, "r", encoding="utf-8") as handle:
            return cls.from_dict(json.load(handle))


@dataclass(frozen=True)
class Projectile:
    hit_x: float
    hit_y: float
    incident_angle_deg: float
    momentum: float

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Projectile:
        try:
            position = data["hit_position"]
            projectile = cls(
                hit_x=float(position[0]),
                hit_y=float(position[1]),
                incident_angle_deg=float(data["incident_angle_deg"]),
                momentum=float(data["momentum"]),
            )
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid projectile", detail=str(exc)
            ) from exc
        if not (0.0 <= projectile.incident_angle_deg <= 90.0):
            raise SimulationError(
                "E_INPUT_INVALID", "incident angle must be in [0, 90]"
            )
        if projectile.momentum < 0:
            raise SimulationError("E_INPUT_INVALID", "momentum cannot be negative")
        return projectile

    def to_dict(self) -> dict[str, Any]:
        return {
            "hit_position": [self.hit_x, self.hit_y],
            "incident_angle_deg": self.incident_angle_deg,
            "momentum": self.momentum,
        }


@dataclass(frozen=True)
class ShotRecord:
    record_id: str
    player_id: str
    client_seq: int
    local_time_ms: int
    target_step: int
    arrival_step: int
    arrival_seq: int
    projectile: Projectile
    prediction: Mapping[str, int]
    note: str = ""

    @property
    def key(self) -> tuple[str, int]:
        return (self.player_id, self.client_seq)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ShotRecord:
        try:
            record = cls(
                record_id=str(data["record_id"]),
                player_id=str(data["player_id"]),
                client_seq=int(data["client_seq"]),
                local_time_ms=int(data["local_time_ms"]),
                target_step=int(data["target_step"]),
                arrival_step=int(data["arrival_step"]),
                arrival_seq=int(data["arrival_seq"]),
                projectile=Projectile.from_dict(data["projectile"]),
                prediction={
                    str(cover_id): to_micro(amount)
                    for cover_id, amount in dict(data.get("prediction", {})).items()
                },
                note=str(data.get("note", "")),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid shot record", detail=str(exc)
            ) from exc
        if record.target_step < 1 or record.arrival_step < 1:
            raise SimulationError(
                "E_INPUT_INVALID", "shot steps must be positive", shot_id=record.record_id
            )
        if record.client_seq < 1 or record.arrival_seq < 0:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid sequence", shot_id=record.record_id
            )
        if any(amount < 0 for amount in record.prediction.values()):
            raise SimulationError(
                "E_INPUT_INVALID", "prediction cannot be negative", shot_id=record.record_id
            )
        return record

    def to_dict(self) -> dict[str, Any]:
        return {
            "record_id": self.record_id,
            "player_id": self.player_id,
            "client_seq": self.client_seq,
            "local_time_ms": self.local_time_ms,
            "target_step": self.target_step,
            "arrival_step": self.arrival_step,
            "arrival_seq": self.arrival_seq,
            "projectile": self.projectile.to_dict(),
            "prediction": {
                cover_id: from_micro(amount)
                for cover_id, amount in sorted(self.prediction.items())
            },
            "note": self.note,
        }


@dataclass(frozen=True)
class RevocationCommand:
    revocation_id: str
    player_id: str
    client_seq: int
    arrival_step: int
    arrival_seq: int
    reason: str = ""

    @property
    def key(self) -> tuple[str, int]:
        return (self.player_id, self.client_seq)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> RevocationCommand:
        try:
            command = cls(
                revocation_id=str(data["revocation_id"]),
                player_id=str(data["player_id"]),
                client_seq=int(data["client_seq"]),
                arrival_step=int(data["arrival_step"]),
                arrival_seq=int(data["arrival_seq"]),
                reason=str(data.get("reason", "")),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid revocation", detail=str(exc)
            ) from exc
        if command.client_seq < 1 or command.arrival_step < 1:
            raise SimulationError(
                "E_INPUT_INVALID", "invalid revocation sequence"
            )
        return command

    def to_dict(self) -> dict[str, Any]:
        return {
            "revocation_id": self.revocation_id,
            "player_id": self.player_id,
            "client_seq": self.client_seq,
            "arrival_step": self.arrival_step,
            "arrival_seq": self.arrival_seq,
            "reason": self.reason,
        }


def load_shots(path: str) -> tuple[dict[str, Any], list[ShotRecord]]:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    try:
        shots = [ShotRecord.from_dict(item) for item in data["shots"]]
    except (KeyError, TypeError, ValueError) as exc:
        raise SimulationError("E_INPUT_INVALID", "invalid shots file", detail=str(exc)) from exc
    return data, shots


def load_revocations(path: str) -> list[RevocationCommand]:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    try:
        return [
            RevocationCommand.from_dict(item)
            for item in data.get("revocations", [])
        ]
    except (KeyError, TypeError, ValueError) as exc:
        raise SimulationError(
            "E_INPUT_INVALID", "invalid revocations file", detail=str(exc)
        ) from exc


def sum_predictions(predictions: Iterable[Mapping[str, int]]) -> dict[str, int]:
    total: dict[str, int] = {}
    for prediction in predictions:
        for cover_id, amount in prediction.items():
            total[cover_id] = total.get(cover_id, 0) + int(amount)
    return total

