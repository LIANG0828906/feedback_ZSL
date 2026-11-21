"""Deterministic local scenario and baseline generator."""

from __future__ import annotations

import hashlib
import json
import math
import random
import shutil
from decimal import Decimal
from pathlib import Path
from typing import Any

from .engine import ReplayEngine, scenario_fingerprint
from .errors import E_REPLAY_INVALID, ReplayError
from .loader import load_scenario
from .snapshot import atomic_write_bytes, canonical_json_bytes


def _decimal_text(value: Decimal, places: int = 3) -> str:
    return f"{value:.{places}f}"


def _json_document(value: Any) -> bytes:
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")


def _write_document(path: Path, value: Any) -> None:
    atomic_write_bytes(path, _json_document(value))


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _prepare_output(output_dir: Path, clean: bool) -> None:
    if output_dir.exists():
        if not output_dir.is_dir():
            raise ReplayError(
                E_REPLAY_INVALID,
                "generator output path must be a directory",
                None,
                "output",
            )
        if not clean and any(output_dir.iterdir()):
            raise ReplayError(
                E_REPLAY_INVALID,
                "generator output directory is not empty; use --clean",
                None,
                "output",
            )
        if clean:
            shutil.rmtree(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)


def _build_covers(
    count: int,
    rng: random.Random,
    memory_budget: int,
) -> list[dict[str, Any]]:
    columns = max(1, math.ceil(math.sqrt(count)))
    covers: list[dict[str, Any]] = []
    width = Decimal(10)
    height = Decimal(8)
    gap = Decimal("1.5")
    for index in range(count):
        row, column = divmod(index, columns)
        min_x = Decimal(column) * (width + gap) + Decimal(1)
        min_y = Decimal(row) * (height + gap) + Decimal(1)
        max_x = min_x + width
        max_y = min_y + height
        hardness = Decimal(rng.randint(1000, 2200)) / Decimal(1000)
        durability = Decimal(rng.randint(100, 160))
        threshold = Decimal(rng.randint(18, 34))
        if threshold >= durability:
            threshold = durability / Decimal(2)
        radius = Decimal(13)
        buffer_bytes = rng.randint(2400, 7200)
        if index == 0:
            buffer_bytes = max(memory_budget + 1024, buffer_bytes)
        covers.append(
            {
                "id": f"C{index + 1:02d}",
                "bounds": {
                    "min_x": _decimal_text(min_x),
                    "min_y": _decimal_text(min_y),
                    "max_x": _decimal_text(max_x),
                    "max_y": _decimal_text(max_y),
                },
                "center_x": (min_x + max_x) / 2,
                "center_y": (min_y + max_y) / 2,
                "material_hardness": _decimal_text(hardness),
                "max_durability": _decimal_text(durability),
                "segment_damage_threshold": _decimal_text(threshold),
                "attenuation_radius": _decimal_text(radius),
                "temporary_buffer_bytes": buffer_bytes,
            }
        )
    for cover in covers:
        neighbors = []
        for candidate in covers:
            if candidate["id"] == cover["id"]:
                continue
            distance = math.hypot(
                float(cover["center_x"] - candidate["center_x"]),
                float(cover["center_y"] - candidate["center_y"]),
            )
            if distance <= float(cover["attenuation_radius"]):
                neighbors.append(
                    {
                        "cover_id": candidate["id"],
                        "distance": _decimal_text(Decimal(str(round(distance, 3)))),
                    }
                )
        cover["neighbors"] = sorted(neighbors, key=lambda item: item["cover_id"])
    for cover in covers:
        cover.pop("center_x")
        cover.pop("center_y")
    return covers


def _generate_shots_and_revokes(
    covers: list[dict[str, Any]],
    player_count: int,
    steps: int,
    revoke_density: float,
    rng: random.Random,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    shots: list[dict[str, Any]] = []
    next_client_seq = {f"P{index + 1}": 0 for index in range(player_count)}
    arrival_seq = 1
    shots_per_step = max(1, math.ceil(player_count * 0.8))
    for step in range(steps):
        for _ in range(shots_per_step):
            player_id = f"P{rng.randint(1, player_count)}"
            cover = covers[rng.randrange(len(covers))]
            bounds = cover["bounds"]
            min_x = Decimal(bounds["min_x"])
            max_x = Decimal(bounds["max_x"])
            min_y = Decimal(bounds["min_y"])
            max_y = Decimal(bounds["max_y"])
            hit_x = min_x + (max_x - min_x) * Decimal(rng.randint(100, 900)) / Decimal(1000)
            hit_y = min_y + (max_y - min_y) * Decimal(rng.randint(100, 900)) / Decimal(1000)
            angle = Decimal(rng.randint(0, 72))
            momentum = Decimal(rng.randint(4000, 12500)) / Decimal(1000)
            client_seq = next_client_seq[player_id]
            next_client_seq[player_id] += 1
            shots.append(
                {
                    "player_id": player_id,
                    "client_seq": client_seq,
                    "local_timestamp": step * 1000 + arrival_seq,
                    "arrival_seq": arrival_seq,
                    "hit_position": {
                        "x": _decimal_text(hit_x),
                        "y": _decimal_text(hit_y),
                    },
                    "cover_id": cover["id"],
                    "incidence_angle": _decimal_text(angle),
                    "momentum": _decimal_text(momentum),
                    "effective_step": step,
                }
            )
            arrival_seq += 1
    shots.sort(key=lambda item: item["arrival_seq"])
    revokes: list[dict[str, Any]] = []
    if revoke_density > 0 and shots:
        revoke_count = min(
            len(shots),
            max(1, round(len(shots) * float(revoke_density))),
        )
        selected = rng.sample(shots, revoke_count)
        for index, shot in enumerate(sorted(selected, key=lambda item: item["arrival_seq"])):
            if index % 2 == 0:
                effective_step = int(shot["effective_step"])
            else:
                effective_step = min(steps - 1, int(shot["effective_step"]) + 1)
            revokes.append(
                {
                    "player_id": shot["player_id"],
                    "client_seq": shot["client_seq"],
                    "arrival_seq": arrival_seq,
                    "effective_step": effective_step,
                }
            )
            arrival_seq += 1
    return shots, revokes


def generate_sample(
    output_dir: str | Path,
    *,
    seed: int = 42,
    cover_count: int = 8,
    player_count: int = 3,
    revoke_density: float = 0.2,
    steps: int = 24,
    parallelism: int = 2,
    memory_budget_bytes: int = 16384,
    clean: bool = False,
) -> dict[str, Any]:
    output = Path(output_dir).expanduser().resolve()
    if cover_count < 1:
        raise ReplayError(
            E_REPLAY_INVALID,
            "cover_count must be >= 1",
            None,
            "generator.cover_count",
        )
    if player_count < 1:
        raise ReplayError(
            E_REPLAY_INVALID,
            "player_count must be >= 1",
            None,
            "generator.player_count",
        )
    if not (0 <= revoke_density <= 1):
        raise ReplayError(
            E_REPLAY_INVALID,
            "revoke_density must be in [0, 1]",
            None,
            "generator.revoke_density",
        )
    if steps < 1:
        raise ReplayError(
            E_REPLAY_INVALID,
            "steps must be >= 1",
            None,
            "generator.steps",
        )
    _prepare_output(output, clean)
    rng = random.Random(seed)
    covers = _build_covers(cover_count, rng, memory_budget_bytes)
    shots, revokes = _generate_shots_and_revokes(
        covers,
        player_count,
        steps,
        revoke_density,
        rng,
    )
    config = {
        "schema_version": 1,
        "physical_steps": steps,
        "fixed_point_decimals": 3,
        "parallelism": parallelism,
        "memory_budget_bytes": memory_budget_bytes,
        "degradation_sequence": ["1.0", "0.75", "0.5", "0.25"],
        "crack_max_iterations": 16,
        "crack_propagation_ratio": "0.4",
        "revocation_window_steps": 3,
        "damage_model_version": "cover-damage-v1",
        "baseline_version": "baseline-v1",
        "quantization_mode": "half_even",
    }
    match = {
        "schema_version": 1,
        "config_file": "config.json",
        "covers_file": "covers.json",
        "baseline_file": "baseline.json",
        "shots": shots,
        "revokes": revokes,
    }
    _write_document(output / "config.json", config)
    _write_document(output / "covers.json", covers)
    _write_document(output / "match.json", match)

    scenario = load_scenario(output)
    staging = output / "_baseline_run"
    engine = ReplayEngine(
        scenario,
        staging,
        clean=True,
        compare_baseline=False,
    )
    result = engine.run()
    if result.status != "completed":
        raise ReplayError(
            E_REPLAY_INVALID,
            f"generated baseline replay did not complete: {result.status}",
            None,
            "generator.baseline",
        )
    frames = result.trace_frames
    baseline = {
        "schema_version": 1,
        "baseline_version": config["baseline_version"],
        "damage_model_version": config["damage_model_version"],
        "scenario_fingerprint": scenario_fingerprint(scenario),
        "seed": seed,
        "case": {
            "cover_count": cover_count,
            "player_count": player_count,
            "revoke_density": revoke_density,
            "steps": steps,
        },
        "frames": frames,
    }
    _write_document(output / "baseline.json", baseline)
    atomic_write_bytes(
        output / "trajectory.jsonl",
        b"".join(canonical_json_bytes(frame) + b"\n" for frame in frames),
    )
    shutil.rmtree(staging)
    file_names = (
        "config.json",
        "covers.json",
        "match.json",
        "baseline.json",
        "trajectory.jsonl",
    )
    manifest = {
        "schema_version": 1,
        "seed": seed,
        "parameters": {
            "cover_count": cover_count,
            "player_count": player_count,
            "revoke_density": revoke_density,
            "steps": steps,
            "parallelism": parallelism,
            "memory_budget_bytes": memory_budget_bytes,
        },
        "counts": {
            "covers": len(covers),
            "shots": len(shots),
            "revokes": len(revokes),
            "steps": len(frames),
        },
        "scenario_fingerprint": scenario_fingerprint(scenario),
        "files_sha256": {name: _sha256(output / name) for name in file_names},
    }
    _write_document(output / "manifest.json", manifest)
    return manifest


DEFAULT_MATRIX = (
    (4, 2, 0.0),
    (8, 2, 0.2),
    (8, 4, 0.5),
    (12, 3, 0.3),
    (16, 4, 0.15),
    (16, 6, 0.4),
)


def generate_matrix(
    output_dir: str | Path,
    *,
    seed: int = 42,
    cases: tuple[tuple[int, int, float], ...] = DEFAULT_MATRIX,
    steps: int = 20,
    clean: bool = False,
) -> list[dict[str, Any]]:
    output = Path(output_dir).expanduser().resolve()
    _prepare_output(output, clean)
    manifests: list[dict[str, Any]] = []
    for index, (cover_count, player_count, revoke_density) in enumerate(cases):
        case_dir = output / (
            f"case_c{cover_count:02d}_p{player_count:02d}"
            f"_r{round(revoke_density * 100):03d}"
        )
        case_seed = seed + index * 1009
        manifest = generate_sample(
            case_dir,
            seed=case_seed,
            cover_count=cover_count,
            player_count=player_count,
            revoke_density=revoke_density,
            steps=steps,
            parallelism=2 if cover_count <= 8 else 3,
            memory_budget_bytes=16384,
            clean=True,
        )
        manifests.append(
            {
                "case": case_dir.name,
                "seed": case_seed,
                "cover_count": cover_count,
                "player_count": player_count,
                "revoke_density": revoke_density,
                "manifest": manifest,
            }
        )
    index_document = {
        "schema_version": 1,
        "seed": seed,
        "case_count": len(manifests),
        "cases": manifests,
    }
    _write_document(output / "matrix_manifest.json", index_document)
    return manifests

