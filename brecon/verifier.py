from __future__ import annotations

import hashlib
from pathlib import Path

from .baseline import check_baseline
from .canonical import compact_json, read_json
from .config import SampleConfig
from .engine import ReplayEngine
from .errors import BreconError
from .io_utils import write_json_atomic
from .lock import RoomBusyLock, room_lock_path
from .recovery import load_snapshot_file, validate_snapshot


def _trace_bytes(engine: ReplayEngine) -> bytes:
    return "".join(
        compact_json(entry) + "\n" for entry in engine.trace_entries
    ).encode("utf-8")


def _latency_bytes(engine: ReplayEngine) -> bytes:
    return (compact_json(engine.latency_payload()) + "\n").encode("utf-8")


def _claim_keys(engine: ReplayEngine) -> list[tuple[str, int]]:
    values = []
    for trace in engine.trace_entries:
        for operation in trace["effective_ops"]:
            if operation["op_type"] == "claim":
                values.append(
                    (operation["player_id"], int(operation["client_seq"]))
                )
    return values


def verify_sample(
    sample_dir: str | Path,
    *,
    recovery_step: int | None = None,
) -> dict[str, Any]:
    config = SampleConfig.load(sample_dir)
    offline_engine = ReplayEngine.from_sample_dir(config.sample_dir).run()
    online_engine = ReplayEngine.from_sample_dir(config.sample_dir).run()

    baseline_result = check_baseline(online_engine)
    if not baseline_result["ok"]:
        return {
            "sample": config.sample_dir.name,
            "seed": config.raw.get("seed"),
            "room_id": config.room_id,
            "players": len(config.players),
            "steps": config.observation.end_step,
            "ok": False,
            "baseline_verified": False,
            "baseline": baseline_result,
        }

    offline_vs_online = {
        "trace_bytes_equal": _trace_bytes(offline_engine)
        == _trace_bytes(online_engine),
        "latency_bytes_equal": _latency_bytes(offline_engine)
        == _latency_bytes(online_engine),
        "state_equal": offline_engine.state == online_engine.state,
        "head_state_equal": offline_engine.trace_entries[-1]["state_hash"]
        == online_engine.trace_entries[-1]["state_hash"],
        "head_chain_equal": offline_engine.trace_entries[-1]["chain_hash"]
        == online_engine.trace_entries[-1]["chain_hash"],
    }

    checkpoint = recovery_step
    if checkpoint is None:
        candidates = list(config.observation.sync_steps)
        checkpoint = candidates[len(candidates) // 2]
    checkpoint = max(1, min(int(checkpoint), config.observation.end_step))
    snapshot_path = config.snapshots_path / f"step_{checkpoint:06d}.json"
    recovery: dict[str, Any] = {
        "snapshot_step": checkpoint,
        "available": snapshot_path.is_file(),
    }
    if snapshot_path.is_file():
        snapshot = load_snapshot_file(snapshot_path, config)
        validate_snapshot(snapshot, config)
        recovered = ReplayEngine.from_snapshot(
            config,
            ReplayEngine._load_operations(config),
            snapshot,
        ).run()
        claim_keys = _claim_keys(recovered)
        recovery.update(
            {
                "chain_equal": recovered.chain == online_engine.chain,
                "trace_bytes_equal": _trace_bytes(recovered)
                == _trace_bytes(online_engine),
                "trace_equal": recovered.trace_entries
                == online_engine.trace_entries,
                "state_equal": recovered.state == online_engine.state,
                "baseline_verified": check_baseline(recovered)["ok"],
                "no_reapplied_claims": len(claim_keys) == len(set(claim_keys)),
                "resumed_from_step": checkpoint,
            }
        )
    else:
        recovery.update(
            {
                "chain_equal": False,
                "trace_bytes_equal": False,
                "trace_equal": False,
                "state_equal": False,
                "baseline_verified": False,
                "no_reapplied_claims": False,
            }
        )

    latency = online_engine.latency_payload()
    trace_bytes = _trace_bytes(online_engine)
    result: dict[str, Any] = {
        "sample": config.sample_dir.name,
        "seed": config.raw.get("seed"),
        "room_id": config.room_id,
        "players": len(config.players),
        "steps": config.observation.end_step,
        "ok": (
            baseline_result["ok"]
            and all(offline_vs_online.values())
            and online_engine.conservation_report()["ok"]
            and recovery["chain_equal"]
            and recovery["trace_bytes_equal"]
            and recovery["state_equal"]
            and recovery["no_reapplied_claims"]
        ),
        "baseline_verified": True,
        "first_divergence": None,
        "chain_entries": len(online_engine.chain),
        "head_state_hash": online_engine.trace_entries[-1]["state_hash"],
        "head_chain_hash": online_engine.trace_entries[-1]["chain_hash"],
        "trace_sha256": hashlib.sha256(trace_bytes).hexdigest(),
        "trace_bytes": len(trace_bytes),
        "sync_observations": latency["sync_observations"],
        "latency": latency["distribution"],
        "compensation": latency["compensation"],
        "divergences": latency["divergences"],
        "conservation": online_engine.conservation_report(),
        "ordering": online_engine.ordering_events,
        "offline_vs_online": offline_vs_online,
        "recovery": recovery,
        "trace": config.trace_file,
    }
    return result


def verify_workspace(
    workspace: str | Path,
    *,
    sample_names: list[str] | None = None,
    write: bool = True,
    recovery_step: int | None = None,
) -> dict[str, Any]:
    workspace_path = Path(workspace).resolve()
    active_path = workspace_path / "active.json"
    if not active_path.is_file():
        raise BreconError(
            "E_WORKSPACE_INVALID",
            f"workspace manifest not found: {active_path}",
            field_path="active.json",
        )
    active = read_json(active_path)
    selected = set(sample_names or [])
    samples = []
    found_names: set[str] = set()
    for description in active.get("samples", []):
        name = description["name"]
        if selected and name not in selected:
            continue
        found_names.add(name)
        sample_dir = workspace_path / description["directory"]
        config = SampleConfig.load(sample_dir)
        with RoomBusyLock(config.room_id, room_lock_path(sample_dir)):
            samples.append(
                verify_sample(sample_dir, recovery_step=recovery_step)
            )
    missing = selected - found_names
    if missing:
        raise BreconError(
            "E_SAMPLE_NOT_FOUND",
            "requested samples are not present in the workspace",
            details={"samples": sorted(missing)},
        )
    report = {
        "ok": all(sample["ok"] for sample in samples) and bool(samples),
        "seed": active.get("samples", [{}])[0].get("seed") if samples else None,
        "profiles": [
            {
                "players": sample["players"],
                "steps": sample["steps"],
                "disorder": next(
                    (
                        description["disorder"]
                        for description in active.get("samples", [])
                        if description["name"] == sample["sample"]
                    ),
                    None,
                ),
            }
            for sample in samples
        ],
        "samples": samples,
    }
    if write:
        write_json_atomic(workspace_path / "verification.json", report)
    return report

