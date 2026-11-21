"""Protocol data model, canonical serialization, and hash construction."""

from __future__ import annotations

import hashlib
import json
from collections import OrderedDict
from dataclasses import dataclass, field
from decimal import Decimal
from typing import Any

from .errors import fail


STATE_MODEL_VERSION = "state-v2"
SUPPORTED_STATE_MODEL_VERSIONS = ("v1", "v2")
BASELINE_VERSION = "baseline-v1"
SNAPSHOT_VERSION = "snapshot-v1"
ZERO_HASH = "0" * 64

# Fixed serialization order is part of the protocol.  Do not derive this from
# dict ordering supplied by a recording or a baseline file.
STATE_TOP_FIELDS = ("state_model_version", "step", "room", "entities")
ROOM_FIELDS = ("resource",)
ENTITY_FIELDS_V1 = ("id", "position", "velocity", "heading")
ENTITY_FIELDS_V2 = ("id", "position", "velocity", "heading", "alive")
VECTOR_FIELDS = ("x", "y")


def dec(value: Any, path: str) -> Decimal:
    """Convert a protocol number to Decimal without binary-float rounding."""

    if isinstance(value, bool):
        fail("E_INPUT_INVALID", "boolean is not a number", path)
    if isinstance(value, Decimal):
        return value
    if isinstance(value, int):
        return Decimal(value)
    if isinstance(value, float):
        # JSON non-finite constants arrive as float.  Preserve them so the
        # validator can emit E_NONFINITE with the exact field path.
        return Decimal(str(value))
    if isinstance(value, str):
        try:
            return Decimal(value)
        except Exception:
            fail("E_INPUT_INVALID", f"invalid decimal string {value!r}", path)
    fail("E_INPUT_INVALID", f"expected finite decimal, got {type(value).__name__}", path)
    raise AssertionError("unreachable")


def require_finite(value: Decimal, path: str, *, nonfinite_code: str = "E_INPUT_INVALID") -> None:
    if not value.is_finite():
        fail(nonfinite_code, "numeric value must be finite", path)


def decimal_string(value: Decimal) -> str:
    """Stable human-readable decimal string used in state and logs."""

    if not value.is_finite():
        raise ValueError("state contains non-finite decimal")
    text = format(value, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    if text in ("", "-0"):
        text = "0"
    return text


def float_string(value: float) -> str:
    """Stable binary64 representation (17 significant digits, round-trip safe)."""

    if value == 0.0:
        return "0"
    return format(value, ".17g")


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=True, separators=(",", ":"), allow_nan=False)


@dataclass(frozen=True)
class Operation:
    op_id: str
    entity_id: str
    client_seq: int
    arrival_seq: int
    arrival_step: int
    effective_step: int
    speed: Decimal
    heading: Decimal
    resource_delta: Decimal
    source_index: int

    def sort_key(self) -> tuple[int, int, int, str, int]:
        # Arrival order is authoritative for operations that become effective
        # in the same step.  Client sequence and protocol index break ties.
        return (
            self.effective_step,
            self.arrival_seq,
            self.client_seq,
            self.op_id,
            self.source_index,
        )

    def identity_dict(self) -> dict[str, Any]:
        return {
            "id": self.op_id,
            "entity_id": self.entity_id,
            "client_seq": self.client_seq,
            "arrival_seq": self.arrival_seq,
            "arrival_step": self.arrival_step,
            "effective_step": self.effective_step,
            "source_index": self.source_index,
        }


@dataclass
class EntityState:
    entity_id: str
    x: Decimal | float
    y: Decimal | float
    vx: Decimal | float
    vy: Decimal | float
    heading: Decimal | float
    alive: bool = True
    flip_count: int = 0

    def clone(self, *, numeric: str) -> "EntityState":
        if numeric == "fixed":
            return EntityState(
                entity_id=self.entity_id,
                x=Decimal(str(self.x)),
                y=Decimal(str(self.y)),
                vx=Decimal(str(self.vx)),
                vy=Decimal(str(self.vy)),
                heading=Decimal(str(self.heading)),
                alive=self.alive,
                flip_count=self.flip_count,
            )
        return EntityState(
            entity_id=self.entity_id,
            x=float(self.x),
            y=float(self.y),
            vx=float(self.vx),
            vy=float(self.vy),
            heading=float(self.heading),
            alive=self.alive,
            flip_count=self.flip_count,
        )


@dataclass
class RoomState:
    resource: Decimal | float
    def clone(self, *, numeric: str) -> "RoomState":
        if numeric == "fixed":
            return RoomState(resource=Decimal(str(self.resource)))
        return RoomState(resource=float(self.resource))


@dataclass
class Config:
    total_steps: int
    timestep: Decimal
    flip_threshold: Decimal
    resource_diff_threshold: Decimal
    coord_limit: Decimal
    fixed_param_decimals: int
    fixed_accum_decimals: int
    late_window_steps: int
    alive_default: bool
    tolerances: dict[str, Decimal]

    def digest_payload(self) -> dict[str, Any]:
        return {
            "total_steps": self.total_steps,
            "timestep": decimal_string(self.timestep),
            "flip_threshold": decimal_string(self.flip_threshold),
            "resource_diff_threshold": decimal_string(self.resource_diff_threshold),
            "coord_limit": decimal_string(self.coord_limit),
            "fixed_param_decimals": self.fixed_param_decimals,
            "fixed_accum_decimals": self.fixed_accum_decimals,
            "late_window_steps": self.late_window_steps,
            "alive_default": self.alive_default,
            "tolerances": {
                key: decimal_string(self.tolerances[key])
                for key in sorted(self.tolerances)
            },
        }


@dataclass
class Recording:
    match_id: str
    source_version: str
    state_model_version: str
    config: Config
    room: RoomState
    entities: dict[str, EntityState]
    operations: list[Operation]
    operations_by_step: dict[int, list[Operation]]
    derived_fields: list[dict[str, Any]] = field(default_factory=list)

    def config_digest(self) -> str:
        payload = {
            "match_id": self.match_id,
            "source_version": self.source_version,
            "state_model_version": self.state_model_version,
            "config": self.config.digest_payload(),
        }
        return hashlib.sha256(canonical_json(payload).encode("ascii")).hexdigest()

    def canonical_operations(self) -> list[dict[str, Any]]:
        return [
            {
                **op.identity_dict(),
                "speed": decimal_string(op.speed),
                "heading": decimal_string(op.heading),
                "resource_delta": decimal_string(op.resource_delta),
            }
            for op in self.operations
        ]


def state_model_from_version(version: str) -> str:
    if version in ("v1", "v2"):
        return f"state-{version}"
    return version


def protocol_version_from_state_model(version: str) -> str:
    if version in ("v1", "v2"):
        return version
    if version.startswith("state-"):
        return version[len("state-") :]
    return version


def entity_fields(version: str = STATE_MODEL_VERSION) -> tuple[str, ...]:
    pv = protocol_version_from_state_model(version)
    if pv == "v1":
        return ENTITY_FIELDS_V1
    if pv == "v2":
        return ENTITY_FIELDS_V2
    fail("E_BASELINE_VERSION", f"unsupported state model version {version!r}", "state_model_version")
    raise AssertionError("unreachable")


def _state_value_string(value: Decimal | float, numeric: str) -> str:
    if numeric == "fixed":
        return decimal_string(Decimal(str(value)))
    return float_string(float(value))


def state_to_dict(
    step: int,
    room: RoomState,
    entities: dict[str, EntityState],
    *,
    numeric: str,
    version: str = STATE_MODEL_VERSION,
) -> OrderedDict[str, Any]:
    pv = protocol_version_from_state_model(version)
    top: OrderedDict[str, Any] = OrderedDict()
    top["state_model_version"] = version
    top["step"] = int(step)
    top["room"] = OrderedDict(
        (name, _state_value_string(room.resource, numeric)) for name in ROOM_FIELDS
    )
    ordered_entities: list[OrderedDict[str, Any]] = []
    for entity_id in sorted(entities):
        entity = entities[entity_id]
        item: OrderedDict[str, Any] = OrderedDict()
        item["id"] = entity.entity_id
        item["position"] = OrderedDict(
            (name, _state_value_string(getattr(entity, name), numeric))
            for name in VECTOR_FIELDS
        )
        item["velocity"] = OrderedDict(
            (name, _state_value_string(getattr(entity, "v" + name), numeric))
            for name in VECTOR_FIELDS
        )
        item["heading"] = _state_value_string(entity.heading, numeric)
        if pv == "v2":
            item["alive"] = bool(entity.alive)
        ordered_entities.append(item)
    top["entities"] = ordered_entities
    return top


def canonical_state_json(
    step: int,
    room: RoomState,
    entities: dict[str, EntityState],
    *,
    numeric: str,
    version: str = STATE_MODEL_VERSION,
) -> str:
    return canonical_json(state_to_dict(step, room, entities, numeric=numeric, version=version))


def state_hash(
    step: int,
    room: RoomState,
    entities: dict[str, EntityState],
    *,
    numeric: str,
    version: str = STATE_MODEL_VERSION,
) -> str:
    canonical = canonical_state_json(step, room, entities, numeric=numeric, version=version)
    return hashlib.sha256(canonical.encode("ascii")).hexdigest()


def chain_hash(prefix: str, state_digest: str) -> str:
    return hashlib.sha256((prefix + "\n" + state_digest).encode("ascii")).hexdigest()


def entity_state_from_dict(item: dict[str, Any], *, numeric: str, version: str) -> EntityState:
    try:
        entity_id = item["id"]
        x = item["position"]["x"]
        y = item["position"]["y"]
        vx = item["velocity"]["x"]
        vy = item["velocity"]["y"]
        heading = item["heading"]
    except (KeyError, TypeError) as exc:
        fail("E_RECOVERY_INVALID", "snapshot state is missing a required entity field", "state.entities")
        raise AssertionError(exc)
    pv = protocol_version_from_state_model(version)
    if pv == "v2":
        if "alive" not in item:
            fail("E_RECOVERY_INVALID", "snapshot entity is missing alive", "state.entities.alive")
        alive = bool(item["alive"])
    else:
        alive = True
    if numeric == "fixed":
        return EntityState(
            entity_id=str(entity_id),
            x=dec(x, "state.entities.position.x"),
            y=dec(y, "state.entities.position.y"),
            vx=dec(vx, "state.entities.velocity.x"),
            vy=dec(vy, "state.entities.velocity.y"),
            heading=dec(heading, "state.entities.heading"),
            alive=alive,
        )
    return EntityState(
        entity_id=str(entity_id),
        x=float(dec(x, "state.entities.position.x")),
        y=float(dec(y, "state.entities.position.y")),
        vx=float(dec(vx, "state.entities.velocity.x")),
        vy=float(dec(vy, "state.entities.velocity.y")),
        heading=float(dec(heading, "state.entities.heading")),
        alive=alive,
    )


def state_from_dict(
    payload: dict[str, Any],
    *,
    numeric: str,
    expected_version: str | None = None,
) -> tuple[int, RoomState, dict[str, EntityState]]:
    if not isinstance(payload, dict):
        fail("E_RECOVERY_INVALID", "state must be an object", "state")
    version = payload.get("state_model_version", expected_version)
    if expected_version is not None and version != expected_version:
        fail("E_RECOVERY_INVALID", "state model version mismatch", "state.state_model_version")
    try:
        step = int(payload["step"])
        room_payload = payload["room"]
        resource = room_payload["resource"]
        entity_payloads = payload["entities"]
    except (KeyError, TypeError, ValueError):
        fail("E_RECOVERY_INVALID", "state is missing required fields", "state")
        raise AssertionError("unreachable")
    if numeric == "fixed":
        room = RoomState(resource=dec(resource, "state.room.resource"))
    else:
        room = RoomState(resource=float(dec(resource, "state.room.resource")))
    entities: dict[str, EntityState] = {}
    if not isinstance(entity_payloads, list):
        fail("E_RECOVERY_INVALID", "state.entities must be an array", "state.entities")
    for item in entity_payloads:
        entity = entity_state_from_dict(item, numeric=numeric, version=str(version))
        if entity.entity_id in entities:
            fail("E_RECOVERY_INVALID", "duplicate entity in state", "state.entities")
        entities[entity.entity_id] = entity
    return step, room, entities


def serialization_order() -> dict[str, list[str]]:
    return {
        "state": list(STATE_TOP_FIELDS),
        "room": list(ROOM_FIELDS),
        "entity_v1": list(ENTITY_FIELDS_V1),
        "entity_v2": list(ENTITY_FIELDS_V2),
        "vector": list(VECTOR_FIELDS),
    }
