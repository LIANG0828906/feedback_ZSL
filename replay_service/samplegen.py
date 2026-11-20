"""Deterministic sample and baseline generator.

Same seed + parameters produce byte-identical samples, baselines and
traces. No wall clock, no network, no external data.
"""

from __future__ import annotations

import json
import os
import random

from .model import MODEL_VERSION

OP_WEIGHTS = (("move", 60), ("vel", 15), ("resource", 15),
              ("kill", 5), ("revive", 5))


def _payload(rng, kind):
    if kind == "move":
        return {"dx": rng.randint(-5, 5), "dy": rng.randint(-5, 5)}
    if kind == "vel":
        return {"vx": rng.randint(-3, 3), "vy": rng.randint(-3, 3)}
    if kind == "resource":
        return {"delta": rng.randint(-2, 4)}
    return {}


def generate_sample(seed, players, steps, disorder, out_dir, room="room"):
    rng = random.Random(seed)
    player_ids = [f"p{i}" for i in range(players)]
    seqs = {p: 0 for p in player_ids}
    ops = []
    for tick in range(1, steps + 1):
        for player in player_ids:
            if rng.random() < 0.7:
                kind = rng.choices([k for k, _ in OP_WEIGHTS],
                                   weights=[w for _, w in OP_WEIGHTS])[0]
                seqs[player] += 1
                ops.append({"player": player, "seq": seqs[player],
                            "tick": tick, "kind": kind,
                            "payload": _payload(rng, kind)})
    # Deterministic out-of-order arrival: bounded local swaps driven by
    # the seeded RNG. Arrival order is the list order in the file.
    window = max(2, int(disorder * 16))
    for i in range(len(ops)):
        if rng.random() < disorder:
            j = min(len(ops) - 1, i + rng.randint(1, window))
            ops[i], ops[j] = ops[j], ops[i]
    initial_state = {
        "entities": {p: {"x": i * 10, "y": -i * 10, "vx": 0, "vy": 0,
                         "alive": True}
                     for i, p in enumerate(player_ids)},
        "resources": 100,
    }
    sample = {"version": MODEL_VERSION, "seed": seed, "room": room,
              "players": players, "steps": steps, "disorder": disorder,
              "initial_state": initial_state, "ops": ops}
    os.makedirs(out_dir, exist_ok=True)
    sample_path = os.path.join(out_dir, "sample.json")
    with open(sample_path, "w", encoding="utf-8") as fh:
        json.dump(sample, fh, sort_keys=True, separators=(",", ":"))
        fh.write("\n")
    return sample_path


def write_baseline(trace_path, baseline_path, meta):
    """Build a baseline file from a reference run's trace log."""
    with open(trace_path, "r", encoding="utf-8") as fh:
        trace = [json.loads(ln) for ln in fh if ln.strip()]
    header = {"version": MODEL_VERSION, "kind": "baseline", **meta,
              "steps": len(trace)}
    with open(baseline_path, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(header, sort_keys=True,
                            separators=(",", ":")) + "\n")
        for entry in trace:
            fh.write(json.dumps(
                {"step": entry["step"], "state_hash": entry["state_hash"],
                 "chain_hash": entry["chain_hash"]},
                sort_keys=True, separators=(",", ":")) + "\n")
    return baseline_path
