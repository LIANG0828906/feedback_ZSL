"""Local-only deterministic sample generator."""

from __future__ import annotations

import hashlib
import json
import os
import random
import tempfile
from pathlib import Path
from typing import Any

from .model import InitialState, RevocationCommand, ShotRecord, from_micro
from .physics import prediction_from_initial
from .simulation import Simulation


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=path.parent,
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2, allow_nan=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def _cover_layout(count: int) -> list[dict[str, Any]]:
    columns = 3
    covers: list[dict[str, Any]] = []
    for index in range(count):
        row, column = divmod(index, columns)
        covers.append(
            {
                "cover_id": f"C{index + 1:02d}",
                "x": round(column * 1.55, 3),
                "y": round(row * 1.45, 3),
                "width": 1.2,
                "height": 1.0,
                "hardness": 2.0,
                "max_durability": 120.0,
                "segment_threshold": 28.0,
            }
        )
    return covers


def _make_initial(
    *,
    seed: int,
    cover_count: int,
    start_step: int,
    end_step: int,
    step_ms: int,
    compensation_window: int,
) -> dict[str, Any]:
    rng = random.Random(seed ^ 0x5EED)
    covers = _cover_layout(cover_count)
    for cover in covers:
        cover["hardness"] = round(1.4 + rng.randrange(0, 8) * 0.2, 2)
        durability = 240 + rng.randrange(0, 6) * 35
        cover["max_durability"] = float(durability)
        cover["segment_threshold"] = float(
            max(36, round(durability * (0.20 + rng.randrange(0, 4) * 0.02)))
        )
    return {
        "schema_version": 1,
        "match_id": f"local-cover-{seed}-{cover_count}",
        "step_ms": step_ms,
        "observation": {
            "start_step": start_step,
            "end_step": end_step,
            "sample_rule": (
                "one snapshot after all arrivals and due settlements at each "
                "fixed physics-step boundary; end_step is inclusive"
            ),
        },
        "truth_source": "initial_state.json",
        "compensation_window_steps": compensation_window,
        "spread_radius": 1.85,
        "chain_decay": 0.42,
        "max_chain_depth": 2,
        "covers": covers,
    }


def _record_dict(
    *,
    record_id: str,
    player_id: str,
    client_seq: int,
    local_time_ms: int,
    target_step: int,
    arrival_step: int,
    cover: dict[str, Any],
    rng: random.Random,
    prediction_scale: float = 1.0,
    note: str = "",
    initial: InitialState | None = None,
) -> dict[str, Any]:
    x = cover["x"] + cover["width"] * rng.uniform(0.2, 0.8)
    y = cover["y"] + cover["height"] * rng.uniform(0.2, 0.8)
    projectile = {
        "hit_position": [round(x, 4), round(y, 4)],
        "incident_angle_deg": float(rng.choice([0, 5, 10, 15, 20, 25, 30, 35])),
        "momentum": float(55 + rng.randrange(0, 95)),
    }
    if initial is not None:
        from .model import Projectile

        prediction = prediction_from_initial(
            initial, Projectile.from_dict(projectile)
        )
    else:
        prediction = {}
    return {
        "record_id": record_id,
        "player_id": player_id,
        "client_seq": client_seq,
        "local_time_ms": local_time_ms,
        "target_step": target_step,
        "arrival_step": arrival_step,
        "arrival_seq": 0,
        "projectile": projectile,
        "prediction": {
            cover_id: round(from_micro(amount) * prediction_scale, 6)
            for cover_id, amount in sorted(prediction.items())
            if amount > 0
        },
        "note": note,
    }


def _build_shots(
    *,
    seed: int,
    initial: InitialState,
    shot_count: int,
    end_step: int,
    include_anomalies: bool,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], dict[str, Any]]:
    rng = random.Random(seed)
    step_ms = initial.step_ms
    covers = [cover.to_dict() for cover_id in initial.cover_ids for cover in [initial.covers[cover_id]]]
    records: list[dict[str, Any]] = []
    player_sequence = {"P1": 0, "P2": 0}
    drift_indices = {7, 23} if shot_count > 25 else {3}
    for index in range(shot_count):
        player = "P1" if index % 2 == 0 else "P2"
        player_sequence[player] += 1
        client_seq = player_sequence[player]
        target_step = rng.randint(2, max(3, end_step - 5))
        cover = covers[rng.randrange(0, len(covers))]
        arrival_delta = rng.choice([-2, -1, 0, 0, 0, 0, 1, 2])
        arrival_step = max(1, target_step + arrival_delta)
        prediction_scale = 1.28 if index in drift_indices else 1.0
        note = "prediction-drift" if prediction_scale != 1.0 else ""
        record = _record_dict(
            record_id=f"shot-{player}-{client_seq:04d}",
            player_id=player,
            client_seq=client_seq,
            local_time_ms=target_step * step_ms + rng.randrange(0, step_ms),
            target_step=target_step,
            arrival_step=arrival_step,
            cover=cover,
            rng=rng,
            prediction_scale=prediction_scale,
            note=note,
            initial=initial,
        )
        records.append(record)

    anomalies: dict[str, Any] = {}
    if include_anomalies:
        for index, (offset, label) in enumerate([(5, "late-5"), (7, "late-7")]):
            player = "P2" if index % 2 else "P1"
            player_sequence[player] += 1
            client_seq = player_sequence[player]
            target = 3 + index * 2
            arrival = min(end_step, target + offset)
            cover = covers[(index + 2) % len(covers)]
            records.append(
                _record_dict(
                    record_id=f"late-{label}-{player}-{client_seq:04d}",
                    player_id=player,
                    client_seq=client_seq,
                    local_time_ms=target * step_ms + index,
                    target_step=target,
                    arrival_step=arrival,
                    cover=cover,
                    rng=rng,
                    note=f"late-by-{offset}",
                    initial=initial,
                )
            )

        player = "P1"
        player_sequence[player] += 1
        client_seq = player_sequence[player]
        records.append(
            _record_dict(
                record_id=f"late-window-{player}-{client_seq:04d}",
                player_id=player,
                client_seq=client_seq,
                local_time_ms=14 * step_ms,
                target_step=14,
                arrival_step=16,
                cover=covers[1],
                rng=rng,
                note="late-within-window",
                initial=initial,
            )
        )
        duplicate_sources = [records[2], records[min(9, len(records) - 1)]]
        for duplicate_index, source in enumerate(duplicate_sources):
            duplicate = dict(source)
            duplicate["record_id"] = f"{source['record_id']}-dup{duplicate_index + 1}"
            duplicate["arrival_step"] = min(
                end_step, int(source["arrival_step"]) + duplicate_index
            )
            duplicate["note"] = "duplicate-delivery"
            records.append(duplicate)

    revocations: list[dict[str, Any]] = []
    if include_anomalies:
        early_candidates = [
            record
            for record in records
            if int(record["arrival_step"]) + 2 <= int(record["target_step"])
            and "dup" not in record["record_id"]
        ]
        if not early_candidates:
            candidate = records[0]
            candidate["arrival_step"] = max(1, int(candidate["target_step"]) - 2)
            early_candidates = [candidate]
        accepted_target = early_candidates[len(early_candidates) // 2]
        revocations.append(
            {
                "revocation_id": "revoke-open-001",
                "player_id": accepted_target["player_id"],
                "client_seq": accepted_target["client_seq"],
                "arrival_step": accepted_target["target_step"],
                "arrival_seq": 0,
                "reason": "compensation-window retraction drill",
            }
        )
        anomalies["accepted_revocation_shot"] = accepted_target["record_id"]

        settled_candidates = [
            record
            for record in records
            if int(record["target_step"]) <= 8
            and record["record_id"] != accepted_target["record_id"]
            and "dup" not in record["record_id"]
        ]
        closed_target = settled_candidates[0] if settled_candidates else records[0]
        revocations.append(
            {
                "revocation_id": "revoke-closed-001",
                "player_id": closed_target["player_id"],
                "client_seq": closed_target["client_seq"],
                "arrival_step": min(end_step, int(closed_target["target_step"]) + 6),
                "arrival_seq": 0,
                "reason": "already-settled retraction drill",
            }
        )
        anomalies["closed_revocation_shot"] = closed_target["record_id"]

    events: list[tuple[int, int, str, str, dict[str, Any]]] = []
    for record in records:
        events.append(
            (
                int(record["arrival_step"]),
                1,
                str(record["record_id"]),
                "shot",
                record,
            )
        )
    for revocation in revocations:
        events.append(
            (
                int(revocation["arrival_step"]),
                0,
                str(revocation["revocation_id"]),
                "revocation",
                revocation,
            )
        )
    events.sort(key=lambda item: (item[0], item[1], item[2]))
    for arrival_seq, (_, _, _, _, event) in enumerate(events, start=1):
        event["arrival_seq"] = arrival_seq
    for record in records:
        record["latency_steps"] = int(record["arrival_step"]) - int(record["target_step"])
        record["scheduled_time_ms"] = int(record["target_step"]) * step_ms
        record["clock_source"] = "sample_generator"
        record["latency_rule"] = "arrival_step - target_step"
    records.sort(key=lambda item: (item["arrival_step"], item["arrival_seq"], item["record_id"]))
    revocations.sort(
        key=lambda item: (item["arrival_step"], item["arrival_seq"], item["revocation_id"])
    )
    anomalies["late_rejections_expected"] = 2
    anomalies["duplicate_deliveries_expected"] = 2
    anomalies["prediction_drift_shots"] = len(drift_indices)
    return records, revocations, anomalies


def _file_sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def generate_sample(
    output_dir: str | os.PathLike[str],
    *,
    seed: int = 20261122,
    cover_count: int = 8,
    shot_count: int = 48,
    end_step: int = 45,
    step_ms: int = 50,
    compensation_window: int = 3,
    include_anomalies: bool = True,
) -> dict[str, Any]:
    if not (3 <= cover_count <= 12):
        raise ValueError("cover_count must be between 3 and 12")
    if shot_count < 4:
        raise ValueError("shot_count must be at least 4")
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    initial_payload = _make_initial(
        seed=seed,
        cover_count=cover_count,
        start_step=1,
        end_step=end_step,
        step_ms=step_ms,
        compensation_window=compensation_window,
    )
    initial = InitialState.from_dict(initial_payload)
    records, revocations, anomalies = _build_shots(
        seed=seed,
        initial=initial,
        shot_count=shot_count,
        end_step=end_step,
        include_anomalies=include_anomalies,
    )
    shots_payload = {
        "schema_version": 1,
        "match_id": initial.match_id,
        "seed": seed,
        "step_ms": step_ms,
        "observation": initial_payload["observation"],
        "arrival_order_rule": (
            "arrival_seq is a global stable integer assigned by the generator; "
            "same-step revocations are applied before the settlement barrier "
            "in arrival_seq order"
        ),
        "clock_rule": (
            "local_time_ms originates in the sample generator; no wall clock is "
            "read by replay, hashing or reconciliation"
        ),
        "latency_rule": (
            "latency_steps is emitted explicitly by the sample generator as "
            "arrival_step - target_step; the workbench does not infer latency"
        ),
        "shots": records,
    }
    revocations_payload = {
        "schema_version": 1,
        "match_id": initial.match_id,
        "revocations": revocations,
    }
    parsed_shots = [ShotRecord.from_dict(item) for item in records]
    parsed_revocations = [
        RevocationCommand.from_dict(item) for item in revocations
    ]
    simulation = Simulation(initial, parsed_shots, parsed_revocations)
    summaries = simulation.run_to()
    baseline = {
        "schema_version": 1,
        "match_id": initial.match_id,
        "seed": seed,
        "initial_hash": initial.initial_hash,
        "observation": initial_payload["observation"],
        "truth_source": initial.truth_source,
        "comparison_rule": (
            "compare every cover's remaining durability, cumulative crack_events "
            "and alive flag at every published physics step"
        ),
        "steps": [
            {
                "step": summary["step"],
                "state_hash": summary["state_hash"],
                "covers": summary["covers"],
                "pending": summary["pending"],
            }
            for summary in summaries
        ],
        "hash_chain": simulation.state.hash_chain,
        "final_hash": (
            simulation.state.hash_chain[-1]["state_hash"]
            if simulation.state.hash_chain
            else None
        ),
        "expected_operational_codes": sorted(
            {report["code"] for report in simulation.state.operational_reports}
        ),
    }
    _write_json(output / "initial_state.json", initial_payload)
    _write_json(output / "shots.json", shots_payload)
    _write_json(output / "revocations.json", revocations_payload)
    _write_json(output / "baseline.json", baseline)
    manifest = {
        "schema_version": 1,
        "seed": seed,
        "cover_count": cover_count,
        "shot_count": len(records),
        "revocation_count": len(revocations),
        "end_step": end_step,
        "initial_hash": initial.initial_hash,
        "anomalies": anomalies,
        "files": {
            name: _file_sha256(output / name)
            for name in (
                "initial_state.json",
                "shots.json",
                "revocations.json",
                "baseline.json",
            )
        },
    }
    _write_json(output / "manifest.json", manifest)
    return manifest

