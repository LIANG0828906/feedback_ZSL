from __future__ import annotations

from pathlib import Path
from typing import Any

from .canonical import (
    GENESIS_CHAIN_HASH,
    MODEL_VERSION,
    SNAPSHOT_VERSION,
    chain_hash,
    compact_json,
    read_json,
    sha256_hex,
    state_hash,
)
from .config import SampleConfig
from .engine import ReplayEngine
from .errors import BreconError

SNAPSHOT_FIELDS = (
    "snapshot_version",
    "model_version",
    "room_id",
    "config_digest",
    "step",
    "buffer_depth",
    "state",
    "buffer",
    "chain",
    "chain_prefix_digest",
    "runtime",
    "checksum",
)

RUNTIME_FIELDS = (
    "seen",
    "settled_claims",
    "trace_steps",
    "state_history",
    "reconciliations",
    "rejections",
    "latency_records",
    "sync_observations",
    "divergences",
    "max_arrival_seq",
)

ENRICHED_HOLD_FIELDS = (
    "local_ts_ms",
    "arrival_ts_ms",
    "arrival_step",
    "arrival_seq",
    "revoke_arrival_step",
)


def _canonical_snapshot_state(snapshot: dict[str, Any]) -> dict[str, Any]:
    states = snapshot.get("runtime", {}).get("state_history", [])
    if states and int(states[-1].get("step", -1)) == int(snapshot.get("step", -2)):
        return states[-1].get("state", {})
    return snapshot.get("state", {})


def _strip_snapshot_enrichment(state: dict[str, Any]) -> dict[str, Any]:
    import copy

    result = copy.deepcopy(state)
    for hold in result.get("holds", []):
        for field in ENRICHED_HOLD_FIELDS:
            hold.pop(field, None)
    for record in result.get("revocations", []):
        record.pop("arrival_seq", None)
    return result


def validate_snapshot(
    snapshot: Any,
    config: SampleConfig,
) -> dict[str, Any]:
    if not isinstance(snapshot, dict):
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot root must be an object",
            step=1,
            field_path="$",
        )
    try:
        step = int(snapshot.get("step", 1) or 1)
    except (TypeError, ValueError) as exc:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot step must be an integer",
            step=1,
            field_path="step",
        ) from exc
    for field in SNAPSHOT_FIELDS:
        if field not in snapshot:
            raise BreconError(
                "E_RECOVERY_INVALID",
                f"snapshot field is missing: {field}",
                step=step,
                field_path=field,
            )
    runtime = snapshot["runtime"]
    if not isinstance(runtime, dict):
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot runtime must be an object",
            step=step,
            field_path="runtime",
        )
    for field in RUNTIME_FIELDS:
        if field not in runtime:
            raise BreconError(
                "E_RECOVERY_INVALID",
                f"snapshot runtime field is missing: {field}",
                step=step,
                field_path=f"runtime.{field}",
            )
    try:
        snapshot_version = int(snapshot["snapshot_version"])
    except (TypeError, ValueError) as exc:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot version must be an integer",
            step=step,
            field_path="snapshot_version",
        ) from exc
    if snapshot_version != SNAPSHOT_VERSION:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "unsupported snapshot version",
            step=step,
            field_path="snapshot_version",
        )
    if str(snapshot["model_version"]) != MODEL_VERSION:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot model version does not match the state model",
            step=step,
            field_path="model_version",
        )
    if str(snapshot["room_id"]) != config.room_id:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot belongs to a different room",
            step=step,
            field_path="room_id",
        )
    if str(snapshot["config_digest"]) != config.digest:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot configuration digest does not match",
            step=step,
            field_path="config_digest",
        )
    if step < 0 or step > config.observation.end_step:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot step is outside the observation range",
            step=step,
            field_path="step",
        )

    checksum_payload = dict(snapshot)
    expected_checksum = checksum_payload.pop("checksum")
    actual_checksum = sha256_hex(compact_json(checksum_payload))
    if str(expected_checksum) != actual_checksum:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot checksum mismatch",
            step=step,
            field_path="checksum",
        )

    chain = snapshot["chain"]
    if not isinstance(chain, list) or (step > 0 and not chain):
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot hash chain is missing",
            step=step,
            field_path="chain",
        )
    previous = GENESIS_CHAIN_HASH
    try:
        first_chain_step = (
            int(chain[0]["step"]) if chain and isinstance(chain[0], dict) else None
        )
    except (KeyError, TypeError, ValueError) as exc:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "hash chain first step is invalid",
            step=1,
            field_path="chain[0].step",
        ) from exc
    if first_chain_step is not None and first_chain_step != config.observation.start_step:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "hash chain does not start at the observation start step",
            step=first_chain_step,
            field_path="chain[0].step",
        )
    for index, entry in enumerate(chain):
        expected_step = config.observation.start_step + index
        if not isinstance(entry, dict):
            raise BreconError(
                "E_RECOVERY_INVALID",
                "hash chain entry is invalid",
                step=expected_step,
                field_path=f"chain[{index}]",
            )
        for field in ("step", "content_hash", "prev_chain_hash", "chain_hash", "buffer_depth"):
            if field not in entry:
                raise BreconError(
                    "E_RECOVERY_INVALID",
                    "hash chain field is missing",
                    step=expected_step,
                    field_path=f"chain[{index}].{field}",
                )
        try:
            chain_step = int(entry["step"])
        except (TypeError, ValueError) as exc:
            raise BreconError(
                "E_RECOVERY_INVALID",
                "hash chain step must be an integer",
                step=expected_step,
                field_path=f"chain[{index}].step",
            ) from exc
        if chain_step != expected_step:
            raise BreconError(
                "E_RECOVERY_INVALID",
                "hash chain step sequence is broken",
                step=expected_step,
                field_path=f"chain[{index}].step",
            )
        if str(entry["prev_chain_hash"]) != previous:
            raise BreconError(
                "E_RECOVERY_INVALID",
                "hash chain predecessor mismatch",
                step=expected_step,
                field_path=f"chain[{index}].prev_chain_hash",
            )
        expected_chain = chain_hash(previous, str(entry["content_hash"]))
        if str(entry["chain_hash"]) != expected_chain:
            raise BreconError(
                "E_RECOVERY_INVALID",
                "hash chain value mismatch",
                step=expected_step,
                field_path=f"chain[{index}].chain_hash",
            )
        previous = str(entry["chain_hash"])
    try:
        final_chain_step = int(chain[-1]["step"])
    except (KeyError, TypeError, ValueError) as exc:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "hash chain head step is invalid",
            step=step,
            field_path="chain",
        ) from exc
    if final_chain_step != step:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot step and chain head do not match",
            step=step,
            field_path="chain",
        )
    prefix_digest = sha256_hex(compact_json(chain))
    if str(snapshot["chain_prefix_digest"]) != prefix_digest:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "hash chain prefix digest mismatch",
            step=step,
            field_path="chain_prefix_digest",
        )

    canonical_state = _canonical_snapshot_state(snapshot)
    if state_hash(canonical_state) != str(chain[-1]["content_hash"]):
        raise BreconError(
            "E_RECOVERY_INVALID",
            "canonical snapshot state does not match the hash chain",
            step=step,
            field_path="runtime.state_history",
        )
    stripped = _strip_snapshot_enrichment(snapshot["state"])
    if compact_json(stripped) != compact_json(canonical_state):
        raise BreconError(
            "E_RECOVERY_INVALID",
            "enriched snapshot state does not match canonical state",
            step=step,
            field_path="state",
        )
    try:
        buffer_depth = int(snapshot["buffer_depth"])
    except (TypeError, ValueError) as exc:
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot buffer depth must be an integer",
            step=step,
            field_path="buffer_depth",
        ) from exc
    if buffer_depth != len(snapshot["buffer"]):
        raise BreconError(
            "E_RECOVERY_INVALID",
            "snapshot buffer depth does not match buffer contents",
            step=step,
            field_path="buffer_depth",
        )
    return canonical_state


def load_snapshot_file(path: Path, config: SampleConfig) -> dict[str, Any]:
    if not path.is_file():
        raise BreconError(
            "E_RECOVERY_INVALID",
            f"snapshot file not found: {path}",
            step=1,
            field_path="snapshot",
        )
    try:
        snapshot = read_json(path)
    except (OSError, ValueError) as exc:
        raise BreconError(
            "E_RECOVERY_INVALID",
            f"snapshot is not valid JSON: {exc}",
            step=1,
            field_path="snapshot",
        ) from exc
    validate_snapshot(snapshot, config)
    return snapshot


def find_latest_valid_snapshot(
    config: SampleConfig,
) -> tuple[dict[str, Any], Path]:
    directory = config.snapshots_path
    if not directory.is_dir():
        raise BreconError(
            "E_RECOVERY_INVALID",
            f"snapshot directory not found: {directory}",
            step=1,
            field_path="snapshots",
        )
    candidates: list[Path] = []
    latest = directory / "latest.json"
    if latest.is_file():
        candidates.append(latest)
    candidates.extend(
        sorted(directory.glob("step_*.json"), reverse=True)
    )
    seen: set[Path] = set()
    first_error: BreconError | None = None
    best: tuple[int, int, dict[str, Any], Path] | None = None
    for preference, path in enumerate(candidates):
        if path in seen:
            continue
        seen.add(path)
        try:
            snapshot = load_snapshot_file(path, config)
            candidate = (
                int(snapshot["step"]),
                -preference,
                snapshot,
                path,
            )
            if best is None or candidate[:2] > best[:2]:
                best = candidate
        except BreconError as exc:
            if first_error is None:
                first_error = exc
    if best is not None:
        return best[2], best[3]
    if first_error is not None:
        raise first_error
    raise BreconError(
        "E_RECOVERY_INVALID",
        "no valid snapshot is available",
        step=1,
        field_path="snapshots",
    )


def recover_engine(
    config: SampleConfig,
    *,
    write_snapshots: bool = False,
) -> tuple[ReplayEngine, Path]:
    snapshot, path = find_latest_valid_snapshot(config)
    operations = ReplayEngine._load_operations(config)
    engine = ReplayEngine.from_snapshot(
        config,
        operations,
        snapshot,
        write_snapshots=write_snapshots,
    )
    return engine, path

