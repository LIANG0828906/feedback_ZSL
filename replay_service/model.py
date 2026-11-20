"""State model, canonical serialization and deterministic hashing.

Determinism rules:
- Integer-only arithmetic (fixed-point), never floats, never wall clock.
- Canonical JSON with sorted keys and tight separators is the single
  source of truth for hashes (never file size or timestamps).
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field, replace

MODEL_VERSION = 3
MAP_BOUND = 1000


@dataclass(frozen=True)
class Operation:
    player: str
    seq: int            # client-side sequence number
    tick: int           # target step declared by the client
    kind: str           # move | vel | kill | revive | resource
    payload: dict
    arrival: int = -1   # server-side arrival order, assigned at ingest

    def with_arrival(self, arrival: int) -> "Operation":
        return replace(self, arrival=arrival)

    def ref(self) -> dict:
        return {"player": self.player, "seq": self.seq,
                "tick": self.tick, "kind": self.kind}

    @staticmethod
    def from_dict(doc: dict) -> "Operation":
        return Operation(player=doc["player"], seq=int(doc["seq"]),
                         tick=int(doc["tick"]), kind=doc["kind"],
                         payload=dict(doc.get("payload", {})))


@dataclass
class Entity:
    x: int
    y: int
    vx: int
    vy: int
    alive: bool


@dataclass
class RoomState:
    entities: dict = field(default_factory=dict)  # player -> Entity
    resources: int = 0

    @staticmethod
    def from_dict(doc: dict) -> "RoomState":
        entities = {p: Entity(x=int(e["x"]), y=int(e["y"]), vx=int(e["vx"]),
                              vy=int(e["vy"]), alive=bool(e["alive"]))
                    for p, e in doc["entities"].items()}
        return RoomState(entities=entities, resources=int(doc["resources"]))


def state_doc(state: RoomState) -> dict:
    """Fixed field order document (canonical form of the room state)."""
    return {
        "entities": {p: {"x": e.x, "y": e.y, "vx": e.vx, "vy": e.vy,
                         "alive": e.alive}
                     for p, e in state.entities.items()},
        "resources": state.resources,
    }


def serialize_state(state: RoomState) -> str:
    return json.dumps(state_doc(state), sort_keys=True, separators=(",", ":"))


def state_hash(state: RoomState) -> str:
    return hashlib.sha256(serialize_state(state).encode("utf-8")).hexdigest()


def genesis_chain() -> str:
    return hashlib.sha256(f"genesis:v{MODEL_VERSION}".encode()).hexdigest()


def chain_link(prev_chain: str, step_state_hash: str) -> str:
    return hashlib.sha256(
        f"{prev_chain}:{step_state_hash}".encode("utf-8")).hexdigest()
