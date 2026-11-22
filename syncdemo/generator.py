"""Deterministic sample generator.

Produces, from a fixed seed (configurable, default DEFAULT_SEED):

* ``units.json``   — initial unit attribute table (one unit per player)
* ``inputs.json``  — per-tick input intent stream (move/skill/gather/item)
* ``clients.json`` — per-tick render frame numbers and latency tiers
* ``truth.json``   — analytic ground truth: key-tick authoritative states,
                     per-client expected visible states, final settlement
* ``manifest.json``— seed, algorithm version, input stream fingerprint

The ground truth is produced by running the authoritative engine and the
client simulation itself, so it always agrees with the verifier.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from . import engine, runner
from .checkpoint import content_hash

DEFAULT_SEED = 20261123
DEFAULT_TICKS = 120
DEFAULT_PLAYERS = 4
KEY_STATE_EVERY = 20


def _gen_units(rng, num_players):
    units = []
    for i in range(num_players):
        units.append({
            "id": f"u{i}",
            "player": f"p{i}",
            "x": round(float(rng.uniform(10.0, 90.0)), 3),
            "y": round(float(rng.uniform(10.0, 90.0)), 3),
            "facing": round(float(rng.uniform(0.0, 360.0)), 3),
            "hp": 100.0,
            "max_hp": 100.0,
            "resource": 0.0,
            "cooldown": 0,
            "speed": 2.5,
        })
    return units


def _gen_inputs(rng, units, ticks):
    ids = [u["id"] for u in units]
    stream = []
    for tick in range(1, ticks + 1):
        intents = []
        for uid in ids:
            roll = float(rng.random())
            if roll < 0.40:
                intents.append({
                    "unit": uid, "type": "move",
                    "dx": round(float(rng.uniform(-3.0, 3.0)), 3),
                    "dy": round(float(rng.uniform(-3.0, 3.0)), 3),
                })
            elif roll < 0.55:
                target = ids[int(rng.integers(0, len(ids)))]
                intents.append({"unit": uid, "type": "skill", "target": target})
            elif roll < 0.80:
                intents.append({"unit": uid, "type": "gather"})
            elif roll < 0.90:
                intents.append({"unit": uid, "type": "item"})
        stream.append({"tick": tick, "intents": intents})
    return stream


def _gen_clients(rng, units, ticks, frames_per_tick):
    base_tiers = [1, 2, 4]
    clients = []
    for i, base in enumerate(base_tiers):
        latency = [
            {
                "tick": tick,
                "tier": base + int(rng.integers(0, 2)),
                "render_frame": tick * frames_per_tick,
            }
            for tick in range(1, ticks + 1)
        ]
        clients.append({
            "id": f"c{i}",
            "player": units[i % len(units)]["player"],
            "latency": latency,
        })
    return clients


def _truth(sample_view, ecfg, rcfg):
    result = runner.run_replay(sample_view, rcfg, ecfg)
    key_states = [
        {"tick": t, "units": result.states[t]["units"]}
        for t in range(0, len(result.states), KEY_STATE_EVERY)
    ]
    if (len(result.states) - 1) % KEY_STATE_EVERY != 0:
        key_states.append(
            {"tick": len(result.states) - 1, "units": result.states[-1]["units"]}
        )
    client_visible = []
    for cid, views in result.client_views.items():
        for i in range(0, len(views), KEY_STATE_EVERY):
            if views[i] is not None:
                client_visible.append(
                    {"client": cid, "tick": i, "units": views[i]["units"]}
                )
    return {
        "key_states": key_states,
        "client_visible": client_visible,
        "settlement": result.settlement,
    }


class _SampleView:
    """Minimal duck-typed Sample for truth generation before files exist."""

    def __init__(self, seed, units, inputs, clients):
        self.seed = seed
        self.units = units
        self.inputs = inputs
        self.clients = clients


def generate(outdir="sample", seed=DEFAULT_SEED, num_players=DEFAULT_PLAYERS,
             ticks=DEFAULT_TICKS, ecfg=None, rcfg=None):
    """Generate a deterministic sample directory.  Returns its path."""
    ecfg = ecfg or engine.EngineConfig()
    rcfg = rcfg or runner.RunConfig()
    rng = np.random.Generator(np.random.PCG64(seed))

    units = _gen_units(rng, num_players)
    inputs = _gen_inputs(rng, units, ticks)
    clients = _gen_clients(rng, units, ticks, rcfg.frame_policy.frames_per_tick)

    sample_view = _SampleView(seed=seed, units=units, inputs=inputs,
                              clients=clients)
    truth = _truth(sample_view, ecfg, rcfg)
    fingerprint = content_hash({"ticks": inputs})

    outdir = Path(outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    (outdir / "units.json").write_text(json.dumps({"units": units}, indent=1))
    (outdir / "inputs.json").write_text(json.dumps({"ticks": inputs}, indent=1))
    (outdir / "clients.json").write_text(json.dumps({"clients": clients},
                                                    indent=1))
    (outdir / "truth.json").write_text(json.dumps(truth, indent=1))
    manifest = {
        "seed": seed,
        "algo_version": ecfg.algo_version,
        "input_fingerprint": fingerprint,
        "ticks": ticks,
        "num_players": num_players,
    }
    (outdir / "manifest.json").write_text(json.dumps(manifest, indent=1))
    return outdir
