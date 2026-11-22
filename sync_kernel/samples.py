from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .engine import (
    MatchEngine,
    compute_input_fingerprint,
    deterministic_roll,
)
from .models import (
    LOGIC_HZ,
    AuthoritativeState,
    MatchContext,
    UnitState,
    build_initial_state,
)
from .protocol import (
    PROTOCOL_SPECS,
    encode_frame,
)
from .util import atomic_write_bytes, canonical_json_bytes


DEFAULT_SEED = 42_869_123
DEFAULT_MATCH_ID = "local-collab-demo"
DEFAULT_TICKS = 60
DEFAULT_COMMIT_WINDOW = 10


@dataclass
class SampleBundle:
    root: Path
    context: MatchContext
    initial_state: AuthoritativeState
    inputs_by_tick: dict[int, list[dict[str, Any]]]
    manifest: dict[str, Any]
    frames_by_version: dict[int, list[dict[str, Any]]]

    def inputs_json(self) -> list[dict[str, Any]]:
        return [
            {"tick": tick, "intents": self.inputs_by_tick[tick]}
            for tick in sorted(self.inputs_by_tick)
        ]

    def frame_bytes(self, version: int, tick: int) -> bytes:
        return canonical_json_bytes(self.frames_by_version[version][tick])


def _initial_units() -> list[UnitState]:
    return [
        UnitState(
            unit_id="u1",
            owner_id="p1",
            team="alpha",
            x=8_000,
            y=8_000,
            heading=0,
            hp=100_000,
            max_hp=100_000,
            cooldown=0,
            resource=50_000,
            status_mask=0,
        ),
        UnitState(
            unit_id="u2",
            owner_id="p1",
            team="alpha",
            x=12_000,
            y=8_000,
            heading=0,
            hp=100_000,
            max_hp=100_000,
            cooldown=0,
            resource=50_000,
            status_mask=0,
        ),
        UnitState(
            unit_id="u3",
            owner_id="p2",
            team="beta",
            x=52_000,
            y=56_000,
            heading=180_000,
            hp=100_000,
            max_hp=100_000,
            cooldown=0,
            resource=50_000,
            status_mask=0,
        ),
        UnitState(
            unit_id="u4",
            owner_id="p2",
            team="beta",
            x=48_000,
            y=56_000,
            heading=180_000,
            hp=100_000,
            max_hp=100_000,
            cooldown=0,
            resource=50_000,
            status_mask=0,
        ),
    ]


def _bounded_roll(seed: int, tick: int, key: str, minimum: int, maximum: int) -> int:
    span = maximum - minimum + 1
    return minimum + deterministic_roll(seed, tick, "sample", key) % span


def generate_inputs(seed: int, ticks: int) -> dict[int, list[dict[str, Any]]]:
    owners = {
        "p1": ["u1", "u2"],
        "p2": ["u3", "u4"],
    }
    opposing = {"p1": ["u3", "u4"], "p2": ["u1", "u2"]}
    inputs: dict[int, list[dict[str, Any]]] = {}
    for tick in range(0, ticks + 1):
        intents: list[dict[str, Any]] = []
        if tick > 0:
            for player_id in sorted(owners):
                for unit_index, unit_id in enumerate(owners[player_id]):
                    if _bounded_roll(seed, tick, f"{unit_id}:still", 0, 9) == 0:
                        dx = 0
                        dy = 0
                    else:
                        dx = _bounded_roll(seed, tick, f"{unit_id}:dx", -700, 700)
                        dy = _bounded_roll(seed, tick, f"{unit_id}:dy", -700, 700)
                    heading_delta = _bounded_roll(
                        seed, tick, f"{unit_id}:heading", -6_000, 6_000
                    )
                    action = None
                    target_id = None
                    if (tick + unit_index * 5) % 17 == 0:
                        action = "strike"
                        target_id = opposing[player_id][
                            _bounded_roll(seed, tick, f"{unit_id}:target", 0, 1)
                        ]
                    elif (tick + unit_index * 3) % 29 == 0:
                        action = "heal"
                    intents.append(
                        {
                            "player_id": player_id,
                            "unit_id": unit_id,
                            "dx": dx,
                            "dy": dy,
                            "heading_delta": heading_delta,
                            "action": action,
                            "target_unit_id": target_id,
                        }
                    )
        inputs[tick] = intents
    return inputs


def build_sample_model(
    *,
    seed: int = DEFAULT_SEED,
    match_id: str = DEFAULT_MATCH_ID,
    ticks: int = DEFAULT_TICKS,
) -> tuple[SampleBundle | None, MatchContext, AuthoritativeState, dict[int, list[dict[str, Any]]], list[AuthoritativeState]]:
    context = MatchContext(
        match_id=match_id,
        seed=seed,
        tick_rate=LOGIC_HZ,
        clock_hz=LOGIC_HZ,
    )
    initial_state = build_initial_state(context, _initial_units())
    inputs = generate_inputs(seed, ticks)
    fingerprint = compute_input_fingerprint(context, initial_state, inputs)
    context = MatchContext(
        match_id=match_id,
        seed=seed,
        tick_rate=LOGIC_HZ,
        clock_hz=LOGIC_HZ,
        input_fingerprint=fingerprint,
    )
    initial_state.match_id = context.match_id
    initial_state.seed = context.seed
    initial_state.rehash()
    engine = MatchEngine(context, initial_state, inputs)
    states, _ = engine.run_to(ticks)
    frames: dict[int, list[dict[str, Any]]] = {}
    for version in sorted(PROTOCOL_SPECS):
        encoded = []
        for state in states:
            previous = states[state.tick - 1] if state.tick > 0 else None
            result = encode_frame(state, version, previous=previous, context=context)
            encoded.append(result.frame)
        frames[version] = encoded
    bundle = SampleBundle(
        root=Path("."),
        context=context,
        initial_state=initial_state,
        inputs_by_tick=inputs,
        manifest={},
        frames_by_version=frames,
    )
    return bundle, context, initial_state, inputs, states


def write_samples(
    root: str | Path,
    *,
    seed: int = DEFAULT_SEED,
    match_id: str = DEFAULT_MATCH_ID,
    ticks: int = DEFAULT_TICKS,
    commit_window: int = DEFAULT_COMMIT_WINDOW,
) -> tuple[SampleBundle, list[AuthoritativeState]]:
    destination = Path(root)
    destination.mkdir(parents=True, exist_ok=True)
    model, context, initial_state, inputs, states = build_sample_model(
        seed=seed,
        match_id=match_id,
        ticks=ticks,
    )
    assert model is not None

    manifest = {
        "format": "deterministic-state-sync-sample",
        "format_version": 1,
        "match_id": context.match_id,
        "seed": context.seed,
        "tick_rate": context.tick_rate,
        "clock_hz": context.clock_hz,
        "ticks": ticks,
        "commit_window": commit_window,
        "players": ["p1", "p2"],
        "units": sorted(initial_state.units),
        "input_fingerprint": context.input_fingerprint,
        "supported_protocol_versions": sorted(PROTOCOL_SPECS),
        "files": {
            "initial_state": "initial_state.json",
            "inputs": "inputs.jsonl",
            "manifest": "manifest.json",
            "protocols": {
                str(version): f"protocols/v{version}.json"
                for version in sorted(PROTOCOL_SPECS)
            },
            "frames": {
                str(version): f"frames_v{version}.jsonl"
                for version in sorted(PROTOCOL_SPECS)
            },
            "expected_states": "expected_states.jsonl",
        },
        "deterministic_notes": [
            "All integer values are fixed point.",
            "Combat rolls are SHA-256 derived from seed, tick and unit id.",
            "No wall clock or process-global RNG participates in simulation.",
        ],
    }
    model.manifest = manifest
    model.root = destination

    atomic_write_bytes(
        destination / "manifest.json",
        canonical_json_bytes(manifest),
    )
    atomic_write_bytes(
        destination / "initial_state.json",
        canonical_json_bytes(
            {
                **context.as_dict(),
                "state": initial_state.as_checkpoint_dict(),
                "units": [unit.as_dict() for unit in initial_state.units.values()],
            }
        ),
    )
    atomic_write_bytes(
        destination / "inputs.jsonl",
        b"\n".join(
            canonical_json_bytes(item)
            for item in [
                {"tick": tick, "intents": inputs[tick]} for tick in sorted(inputs)
            ]
        )
        + b"\n",
    )
    atomic_write_bytes(
        destination / "expected_states.jsonl",
        b"\n".join(
            canonical_json_bytes(
                {
                    "tick": state.tick,
                    "snapshot_hash": state.snapshot_hash,
                    "state": state.authority_payload(),
                }
            )
            for state in states
        )
        + b"\n",
    )

    protocol_dir = destination / "protocols"
    protocol_dir.mkdir(parents=True, exist_ok=True)
    for version, spec in sorted(PROTOCOL_SPECS.items()):
        atomic_write_bytes(
            protocol_dir / f"v{version}.json",
            canonical_json_bytes(
                {
                    **spec,
                    "sample_match_id": context.match_id,
                    "sample_seed": context.seed,
                }
            ),
        )
        atomic_write_bytes(
            destination / f"frames_v{version}.jsonl",
            b"\n".join(
                canonical_json_bytes(frame) for frame in model.frames_by_version[version]
            )
            + b"\n",
        )
    return model, states


def load_samples(root: str | Path) -> SampleBundle:
    source = Path(root)
    manifest = json.loads((source / "manifest.json").read_text(encoding="utf-8"))
    initial_document = json.loads(
        (source / "initial_state.json").read_text(encoding="utf-8")
    )
    context = MatchContext(
        match_id=manifest["match_id"],
        seed=int(manifest["seed"]),
        tick_rate=int(manifest["tick_rate"]),
        clock_hz=int(manifest["clock_hz"]),
        input_fingerprint=manifest["input_fingerprint"],
    )
    initial_state = AuthoritativeState.from_checkpoint_dict(
        initial_document["state"]
    )
    inputs_by_tick: dict[int, list[dict[str, Any]]] = {}
    for line in (source / "inputs.jsonl").read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        item = json.loads(line)
        inputs_by_tick[int(item["tick"])] = item["intents"]
    frames_by_version: dict[int, list[dict[str, Any]]] = {}
    for version in sorted(PROTOCOL_SPECS):
        path = source / f"frames_v{version}.jsonl"
        frames = [
            json.loads(line)
            for line in path.read_text(encoding="utf-8").splitlines()
            if line.strip()
        ]
        frames_by_version[version] = frames
    return SampleBundle(
        root=source,
        context=context,
        initial_state=initial_state,
        inputs_by_tick=inputs_by_tick,
        manifest=manifest,
        frames_by_version=frames_by_version,
    )

