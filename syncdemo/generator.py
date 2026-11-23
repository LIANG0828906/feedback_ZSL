"""Deterministic sample generator.

Produces, from a fixed seed (configurable, default DEFAULT_SEED):

* ``units.json``   — initial unit attribute table (one unit per player)
* ``inputs.json``  — per-tick input intent stream (move/skill/gather/item)
* ``clients.json`` — per-tick render frame numbers and latency tiers
* ``truth.json``   — analytic ground truth: key-tick authoritative states,
                     per-client expected visible states, final settlement
* ``manifest.json``— seed, algorithm version, input stream fingerprint

The ground truth is produced by :mod:`syncdemo.truth`, an independent
reference implementation that shares neither the engine nor the client
simulation code, so ``verify`` genuinely cross-checks both
implementations against each other.
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from . import engine, runner, truth
from .checkpoint import content_hash

DEFAULT_SEED = 20261123
DEFAULT_TICKS = 120
DEFAULT_PLAYERS = 4
KEY_STATE_EVERY = truth.KEY_STATE_EVERY


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


def generate(outdir="sample", seed=DEFAULT_SEED, num_players=DEFAULT_PLAYERS,
             ticks=DEFAULT_TICKS, ecfg=None, rcfg=None):
    """Generate a deterministic sample directory.  Returns its path."""
    ecfg = ecfg or engine.EngineConfig()
    rcfg = rcfg or runner.RunConfig()
    rng = np.random.Generator(np.random.PCG64(seed))

    units = _gen_units(rng, num_players)
    inputs = _gen_inputs(rng, units, ticks)
    clients = _gen_clients(rng, units, ticks, rcfg.frame_policy.frames_per_tick)

    truth_doc = truth.generate_truth(seed, units, inputs, clients, ecfg,
                                     KEY_STATE_EVERY)
    fingerprint = content_hash({"ticks": inputs})

    outdir = Path(outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    (outdir / "units.json").write_text(json.dumps({"units": units}, indent=1))
    (outdir / "inputs.json").write_text(json.dumps({"ticks": inputs}, indent=1))
    (outdir / "clients.json").write_text(json.dumps({"clients": clients},
                                                    indent=1))
    (outdir / "truth.json").write_text(json.dumps(truth_doc, indent=1))
    manifest = {
        "seed": seed,
        "algo_version": ecfg.algo_version,
        "input_fingerprint": fingerprint,
        "ticks": ticks,
        "num_players": num_players,
    }
    (outdir / "manifest.json").write_text(json.dumps(manifest, indent=1))
    return outdir
