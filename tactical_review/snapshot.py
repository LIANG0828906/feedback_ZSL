"""Atomic snapshot publication, hash-chain validation and recovery."""

from __future__ import annotations

import copy
import json
import os
import tempfile
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import SnapshotError
from .hashing import chain_prefix_digest
from .model import GENESIS_HASH, InitialState, RevocationCommand, ShotRecord
from .simulation import Simulation, SimulationState

SNAPSHOT_VERSION = 1
REQUIRED_SNAPSHOT_FIELDS = {
    "snapshot_version",
    "step",
    "match_id",
    "initial_hash",
    "observation",
    "state",
    "summary",
    "hash_chain",
    "chain_prefix_digest",
    "buffer_watermark",
}


@dataclass
class RecoveryResult:
    simulation: Simulation
    snapshot: dict[str, Any]
    summary: dict[str, Any]


class SnapshotStore:
    """One directory per replay branch; every published file is immutable."""

    def __init__(self, directory: str | os.PathLike[str]) -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)

    def snapshot_path(self, step: int) -> Path:
        return self.directory / f"step_{step:06d}.json"

    def list_steps(self) -> list[int]:
        steps: list[int] = []
        for path in self.directory.glob("step_*.json"):
            suffix = path.stem.removeprefix("step_")
            if suffix.isdigit():
                steps.append(int(suffix))
        return sorted(steps)

    def reset(self) -> None:
        for path in self.directory.glob("step_*.json"):
            path.unlink(missing_ok=True)
        for path in self.directory.glob(".*.tmp"):
            path.unlink(missing_ok=True)

    def write(self, simulation: Simulation) -> Path:
        summary = simulation.last_summary
        if summary is None:
            raise SnapshotError(
                "E_SNAPSHOT_WRITE",
                "cannot publish a step without a settled summary",
            )
        state = simulation.state
        if int(summary["step"]) != state.step:
            raise SnapshotError(
                "E_SNAPSHOT_WRITE",
                "summary/state step mismatch",
                step=state.step,
            )
        payload = {
            "snapshot_version": SNAPSHOT_VERSION,
            "step": state.step,
            "match_id": simulation.initial.match_id,
            "initial_hash": simulation.initial.initial_hash,
            "observation": {
                "start_step": simulation.initial.start_step,
                "end_step": simulation.initial.end_step,
                "step_ms": simulation.initial.step_ms,
                "sample_rule": simulation.initial.sample_rule,
                "truth_source": simulation.initial.truth_source,
            },
            "state": state.to_dict(),
            "summary": summary,
            "hash_payload": summary["payload"],
            "hash_chain": list(state.hash_chain),
            "chain_prefix_digest": chain_prefix_digest(state.hash_chain),
            "buffer_watermark": state.buffer_watermark(),
        }
        self._atomic_write(self.snapshot_path(state.step), payload)
        return self.snapshot_path(state.step)

    @staticmethod
    def _atomic_write(path: Path, payload: dict[str, Any]) -> None:
        directory = path.parent
        directory.mkdir(parents=True, exist_ok=True)
        encoded = json.dumps(
            payload,
            ensure_ascii=False,
            indent=2,
            allow_nan=False,
        ).encode("utf-8")
        descriptor, temp_name = tempfile.mkstemp(
            prefix=f".{path.name}.",
            suffix=".tmp",
            dir=directory,
        )
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(encoded)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_name, path)
            try:
                dir_fd = os.open(directory, os.O_RDONLY)
            except OSError:
                dir_fd = None
            if dir_fd is not None:
                try:
                    os.fsync(dir_fd)
                finally:
                    os.close(dir_fd)
        except BaseException:
            try:
                os.unlink(temp_name)
            except OSError:
                pass
            raise

    def _read_json(self, path: Path, fault: str | None, fault_step: int | None) -> dict[str, Any]:
        try:
            with path.open("r", encoding="utf-8") as handle:
                data = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            step = int(path.stem.removeprefix("step_"))
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot file cannot be read",
                step=step,
                path=str(path),
                detail=str(exc),
            ) from exc
        if not isinstance(data, dict):
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot root is not an object",
                step=int(path.stem.removeprefix("step_")),
                path=str(path),
            )
        if fault is not None and (
            fault_step is None or int(data.get("step", -1)) == fault_step
        ):
            data = copy.deepcopy(data)
            if fault == "missing_field":
                data.pop("pending", None)
                state = data.get("state")
                if isinstance(state, dict):
                    state.pop("pending", None)
                else:
                    data.pop("state", None)
            elif fault == "hash_break":
                chain = data.get("hash_chain")
                if isinstance(chain, list) and chain:
                    chain[-1]["previous_hash"] = "f" * 64
                else:
                    data["hash_chain"] = [{"step": data.get("step"), "previous_hash": "f" * 64}]
            elif fault == "durability_tamper":
                covers = data.get("state", {}).get("covers", [])
                if covers:
                    covers[0]["remaining"] = int(covers[0].get("remaining", 0)) + 1
            elif fault == "prefix_digest":
                data["chain_prefix_digest"] = "f" * 64
            else:
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "unknown recovery fault injection",
                    step=data.get("step"),
                    fault=fault,
                )
        return data

    def _validate_snapshot(
        self,
        data: dict[str, Any],
        *,
        path: Path,
        initial: InitialState | None,
        expected_step: int,
        expected_previous: str,
    ) -> tuple[SimulationState, str]:
        step = int(data.get("step", -1))
        missing = sorted(REQUIRED_SNAPSHOT_FIELDS - set(data))
        if missing:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot required fields are missing",
                step=step,
                missing=missing,
                path=str(path),
            )
        if step != expected_step:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot sequence contains a missing step",
                step=expected_step,
                found_step=step,
                path=str(path),
            )
        if int(data["snapshot_version"]) != SNAPSHOT_VERSION:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "unsupported snapshot version",
                step=step,
            )
        if initial is not None:
            if str(data["initial_hash"]) != initial.initial_hash:
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "snapshot initial-state hash does not match sample",
                    step=step,
                )
            if str(data["match_id"]) != initial.match_id:
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "snapshot match_id does not match sample",
                    step=step,
                )
        try:
            state = SimulationState.from_dict(initial, data["state"])
        except SnapshotError:
            raise
        except Exception as exc:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot state cannot be rebuilt",
                step=step,
                detail=str(exc),
            ) from exc
        if state.step != step:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot state step mismatch",
                step=step,
                state_step=state.step,
            )
        chain = data["hash_chain"]
        if not isinstance(chain, list) or len(chain) != step:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "hash-chain prefix length is invalid",
                step=step,
            )
        if expected_step > 1:
            prior_hash = str(chain[expected_step - 2].get("state_hash"))
            if prior_hash != expected_previous:
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "snapshot hash-chain prefix diverges from prior publication",
                    step=expected_step,
                )
        previous = GENESIS_HASH
        for index, link in enumerate(chain, start=1):
            if int(link.get("step", -1)) != index:
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "hash-chain step sequence is broken",
                    step=index,
                )
            if str(link.get("previous_hash")) != previous:
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "hash-chain predecessor is broken",
                    step=index,
                )
            previous = str(link.get("state_hash"))
        if previous != str(chain[-1]["state_hash"]):
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "hash-chain tail is inconsistent",
                step=step,
            )
        try:
            payload, recomputed = Simulation.hash_from_state_dict(data["state"])
        except (KeyError, TypeError, ValueError) as exc:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot hash payload cannot be rebuilt",
                step=step,
                detail=str(exc),
            ) from exc
        if recomputed != str(chain[-1]["state_hash"]):
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot state hash does not match hash chain",
                step=step,
                recomputed=recomputed,
                chain_hash=chain[-1].get("state_hash"),
            )
        if "hash_payload" in data and data["hash_payload"] != payload:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "stored hash payload does not match canonical state",
                step=step,
            )
        if str(data["chain_prefix_digest"]) != chain_prefix_digest(chain):
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot chain-prefix digest is invalid",
                step=step,
            )
        summary = data["summary"]
        if int(summary.get("step", -1)) != step:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot summary step mismatch",
                step=step,
            )
        if str(summary.get("state_hash")) != str(chain[-1]["state_hash"]):
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot summary hash mismatch",
                step=step,
            )
        watermark = data["buffer_watermark"]
        if watermark != state.buffer_watermark():
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "snapshot buffer watermark mismatch",
                step=step,
            )
        return state, previous

    def load_latest(
        self,
        initial: InitialState | None = None,
        *,
        fault: str | None = None,
        fault_step: int | None = None,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        steps = self.list_steps()
        if not steps:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "no published snapshot is available",
                step=0,
                path=str(self.directory),
            )
        if fault is not None and fault_step is None:
            fault_step = steps[-1]
        if fault is not None and fault_step not in steps:
            raise SnapshotError(
                "E_RECOVERY_INVALID",
                "recovery fault targets an unavailable snapshot step",
                step=fault_step,
                available_steps=steps,
            )
        expected_previous = GENESIS_HASH
        last_data: dict[str, Any] | None = None
        last_state: SimulationState | None = None
        for expected_step in range(1, steps[-1] + 1):
            path = self.snapshot_path(expected_step)
            if not path.exists():
                raise SnapshotError(
                    "E_RECOVERY_INVALID",
                    "snapshot sequence contains a gap",
                    step=expected_step,
                    path=str(path),
                )
            data = self._read_json(path, fault, fault_step)
            state, expected_previous = self._validate_snapshot(
                data,
                path=path,
                initial=initial,
                expected_step=expected_step,
                expected_previous=expected_previous,
            )
            last_data = data
            last_state = state
        assert last_data is not None and last_state is not None
        return last_data, last_data["summary"]

    def load_simulation(
        self,
        initial: InitialState,
        shots: Sequence[ShotRecord],
        revocations: Sequence[RevocationCommand] = (),
        *,
        fault: str | None = None,
        fault_step: int | None = None,
    ) -> RecoveryResult:
        snapshot, summary = self.load_latest(
            initial,
            fault=fault,
            fault_step=fault_step,
        )
        state = SimulationState.from_dict(initial, snapshot["state"])
        simulation = Simulation(
            initial,
            shots,
            revocations,
            state=state,
        )
        return RecoveryResult(
            simulation=simulation,
            snapshot=snapshot,
            summary=summary,
        )


def inject_snapshot_fault(
    store: SnapshotStore,
    fault: str,
    step: int | None = None,
) -> Path:
    """Intentionally corrupt one published snapshot for recovery drills."""

    steps = store.list_steps()
    if not steps:
        raise SnapshotError("E_RECOVERY_INVALID", "no snapshot to corrupt", step=0)
    target = steps[-1] if step is None else step
    path = store.snapshot_path(target)
    if not path.exists():
        raise SnapshotError(
            "E_RECOVERY_INVALID", "requested snapshot does not exist", step=target
        )
    with path.open("r", encoding="utf-8") as handle:
        data = json.load(handle)
    if fault == "missing_field":
        data.pop("state", None)
    elif fault == "hash_break":
        data["hash_chain"][-1]["previous_hash"] = "f" * 64
    elif fault == "durability_tamper":
        data["state"]["covers"][0]["remaining"] += 1
    elif fault == "prefix_digest":
        data["chain_prefix_digest"] = "f" * 64
    else:
        raise SnapshotError(
            "E_RECOVERY_INVALID", "unknown snapshot fault", fault=fault, step=target
        )
    path.write_text(
        json.dumps(data, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    return path

