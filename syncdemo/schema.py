"""Sample loading and structural validation.

Validation failures raise SchemaError (E_SCHEMA) with the offending field
name, or UnitRefError (E_UNIT) with the unknown unit identifier.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path

from .engine import UNIT_NUMERIC_FIELDS
from .errors import SchemaError, UnitRefError

REQUIRED_UNIT_FIELDS = ("id", "player") + UNIT_NUMERIC_FIELDS + ("cooldown",)
INTENT_TYPES = ("move", "skill", "gather", "item")


@dataclass
class Sample:
    seed: int
    units: list
    inputs: list
    clients: list
    truth: dict
    meta: dict

    @property
    def unit_ids(self):
        return [u["id"] for u in self.units]

    @property
    def max_tick(self):
        return self.inputs[-1]["tick"] if self.inputs else 0


def _read_json(directory: Path, name: str):
    path = directory / name
    if not path.exists():
        raise SchemaError(f"missing sample file {name}", field=name)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise SchemaError(f"invalid JSON in {name}: {exc}", field=name) from exc


def _require(mapping, key, ctx):
    if key not in mapping:
        raise SchemaError(f"missing field '{key}' in {ctx}", field=key)
    return mapping[key]


def _validate_units(units):
    seen_ids, seen_players = set(), set()
    for unit in units:
        for fname in REQUIRED_UNIT_FIELDS:
            _require(unit, fname, "unit")
        uid = unit["id"]
        if uid in seen_ids:
            raise SchemaError(f"duplicate unit id '{uid}'", field="id")
        seen_ids.add(uid)
        player = unit["player"]
        if player in seen_players:
            raise SchemaError(f"duplicate player id '{player}'", field="player")
        seen_players.add(player)
        for fname in UNIT_NUMERIC_FIELDS:
            value = unit[fname]
            if not isinstance(value, (int, float)) or not math.isfinite(value):
                raise SchemaError(
                    f"unit '{uid}' field '{fname}' is not a finite number",
                    field=fname,
                )
        cooldown = unit["cooldown"]
        if not isinstance(cooldown, int) or cooldown < 0:
            raise SchemaError(
                f"unit '{uid}' field 'cooldown' invalid", field="cooldown"
            )
    return seen_ids


def _validate_inputs(inputs, unit_ids):
    previous = 0
    for entry in inputs:
        tick = _require(entry, "tick", "inputs")
        intents = _require(entry, "intents", "inputs")
        if not isinstance(tick, int) or tick != previous + 1:
            raise SchemaError(
                f"input tick sequence not monotonic/gapless at tick {tick}",
                field="tick",
            )
        previous = tick
        for intent in intents:
            uid = _require(intent, "unit", "intent")
            kind = _require(intent, "type", "intent")
            if uid not in unit_ids:
                raise UnitRefError(
                    f"intent references unknown unit '{uid}'", unit=uid
                )
            if kind not in INTENT_TYPES:
                raise SchemaError(f"unknown intent type '{kind}'", field="type")
            if kind == "skill":
                target = _require(intent, "target", "intent")
                if target not in unit_ids:
                    raise UnitRefError(
                        f"skill references unknown target unit '{target}'",
                        unit=target,
                    )


def _validate_clients(clients, players):
    for client in clients:
        cid = _require(client, "id", "client")
        player = _require(client, "player", "client")
        if player not in players:
            raise UnitRefError(
                f"client '{cid}' references unknown player '{player}'",
                unit=player,
            )
        for obs in _require(client, "latency", "client"):
            tier = _require(obs, "tier", "latency")
            _require(obs, "tick", "latency")
            _require(obs, "render_frame", "latency")
            if not isinstance(tier, int) or tier < 0:
                raise SchemaError(
                    f"client '{cid}' latency tier must be a non-negative int",
                    field="latency",
                )


def _validate_truth(truth, unit_ids):
    for key in ("key_states", "client_visible", "settlement"):
        _require(truth, key, "truth")
    for entry in truth["key_states"]:
        _require(entry, "tick", "truth.key_states")
        for uid in _require(entry, "units", "truth.key_states"):
            if uid not in unit_ids:
                raise UnitRefError(
                    f"truth references unknown unit '{uid}'", unit=uid
                )
    for entry in truth["client_visible"]:
        _require(entry, "client", "truth.client_visible")
        _require(entry, "tick", "truth.client_visible")
        for uid in _require(entry, "units", "truth.client_visible"):
            if uid not in unit_ids:
                raise UnitRefError(
                    f"truth references unknown unit '{uid}'", unit=uid
                )


def validate_sample(sample: Sample) -> Sample:
    unit_ids = _validate_units(sample.units)
    _validate_inputs(sample.inputs, unit_ids)
    players = {u["player"] for u in sample.units}
    _validate_clients(sample.clients, players)
    _validate_truth(sample.truth, unit_ids)
    return sample


def load_sample(directory) -> Sample:
    """Load and validate a generated sample directory."""
    directory = Path(directory)
    manifest = _read_json(directory, "manifest.json")
    units_doc = _read_json(directory, "units.json")
    inputs_doc = _read_json(directory, "inputs.json")
    clients_doc = _read_json(directory, "clients.json")
    truth = _read_json(directory, "truth.json")

    seed = _require(manifest, "seed", "manifest")
    meta = {
        "seed": seed,
        "algo_version": _require(manifest, "algo_version", "manifest"),
        "input_fingerprint": _require(manifest, "input_fingerprint", "manifest"),
    }
    sample = Sample(
        seed=seed,
        units=_require(units_doc, "units", "units.json"),
        inputs=_require(inputs_doc, "ticks", "inputs.json"),
        clients=_require(clients_doc, "clients", "clients.json"),
        truth=truth,
        meta=meta,
    )
    return validate_sample(sample)
