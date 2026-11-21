"""Fixed-step replay state machine with transactional publication."""

from __future__ import annotations

import copy
import hashlib
import os
import shutil
import threading
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from .baseline import BaselineComparator, load_baseline
from .damage import propagation_units
from .errors import (
    E_BASELINE_MISMATCH,
    E_CANCELLED,
    E_CRACK_UNSTABLE,
    E_CREDIT_CONFLICT,
    E_IO,
    E_RECOVERY_INVALID,
    E_REPLAY_INVALID,
    E_REVOKE_CLOSED,
    E_REVOKE_NOT_FOUND,
    ReplayError,
)
from .fixed import format_units, quantize_units
from .model import CoverSpec, ReplayConfig, Revoke, Scenario, Shot
from .scheduler import run_damage_schedule
from .snapshot import (
    GENESIS_CHAIN_HASH,
    atomic_write_bytes,
    canonical_json_bytes,
    compute_chain_hash,
    compute_state_hash,
    load_and_validate_snapshots,
    make_snapshot_document,
    rewrite_trace_from_documents,
    write_snapshot,
)
from .state import (
    CompensationRecord,
    LedgerEntry,
    ReplayState,
    initial_state,
)


@dataclass
class ReplayResult:
    status: str
    steps_completed: int
    state: ReplayState
    trace_frames: list[dict[str, Any]]
    baseline_diff: dict[str, Any] | None = None
    quantization_impact: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "steps_completed": self.steps_completed,
            "last_step": self.state.step,
            "baseline_diff": self.baseline_diff,
            "quantization_impact": self.quantization_impact,
        }


def scenario_fingerprint(scenario: Scenario) -> str:
    return hashlib.sha256(canonical_json_bytes(scenario.normalized())).hexdigest()


def _capacity_units(cover: CoverSpec, config: ReplayConfig) -> int:
    return quantize_units(
        cover.max_durability,
        config.fixed_point_decimals,
        config.quantization_mode,
    )


def _threshold_units(cover: CoverSpec, config: ReplayConfig) -> int:
    return quantize_units(
        cover.segment_damage_threshold,
        config.fixed_point_decimals,
        config.quantization_mode,
    )


class ReplayEngine:
    def __init__(
        self,
        scenario: Scenario,
        output_dir: str | Path,
        *,
        resume: bool = False,
        clean: bool = False,
        compare_baseline: bool = True,
        decimals_override: int | None = None,
        cancel_event: threading.Event | None = None,
        crash_after_step: int | None = None,
    ) -> None:
        if resume and clean:
            raise ReplayError(
                E_REPLAY_INVALID,
                "resume and clean are mutually exclusive",
                None,
                "output",
            )
        if decimals_override is not None and not (0 <= decimals_override <= 9):
            raise ReplayError(
                E_REPLAY_INVALID,
                "fixed-point decimals override must be in [0, 9]",
                None,
                "fixed_point_decimals",
                {"actual": decimals_override},
            )
        self.original_scenario = scenario
        if decimals_override is not None:
            scenario = replace(
                scenario,
                config=replace(
                    scenario.config,
                    fixed_point_decimals=decimals_override,
                ),
            )
        self.scenario = scenario
        self.config = scenario.config
        self.output_dir = Path(output_dir).expanduser().resolve()
        self.snapshot_dir = self.output_dir / "snapshots"
        self.trace_path = self.output_dir / "trajectory.jsonl"
        self.failure_path = self.output_dir / "failure.json"
        self.baseline_diff_path = self.output_dir / "baseline_diff.json"
        self.baseline_report_path = self.output_dir / "baseline_report.json"
        self.fingerprint = scenario_fingerprint(scenario)
        self.cancel_event = cancel_event or threading.Event()
        self.crash_after_step = crash_after_step
        self._lock = threading.RLock()
        self._processing_step: int | None = None
        self._injected_revokes: list[Revoke] = []
        self._injected_counter = 0
        self.baseline_comparator: BaselineComparator | None = None
        self._baseline_diff: dict[str, Any] | None = None
        self._prepare_output(resume=resume, clean=clean)

        if compare_baseline and scenario.baseline_path is not None:
            baseline = load_baseline(scenario.baseline_path)
            self.baseline_comparator = BaselineComparator(scenario, baseline)

        if resume:
            documents, recovered_state, previous_chain = load_and_validate_snapshots(
                self.snapshot_dir,
                self.fingerprint,
            )
            self._documents = documents
            if documents:
                expected_covers = {cover.cover_id for cover in self.covers}
                if set(recovered_state.covers) != expected_covers:
                    raise ReplayError(
                        E_RECOVERY_INVALID,
                        "snapshot cover set does not match scenario",
                        recovered_state.step,
                        "snapshots.state.covers",
                        {
                            "first_break_step": recovered_state.step,
                            "expected": sorted(expected_covers),
                            "actual": sorted(recovered_state.covers),
                        },
                    )
                self._state = recovered_state
                self._previous_chain_hash = previous_chain
                self._injected_revokes = list(recovered_state.pending_injected_revokes)
                rewrite_trace_from_documents(documents, self.trace_path)
                if self.baseline_comparator is not None:
                    for document in documents:
                        self.baseline_comparator.compare(document["frame"])
                    self._baseline_diff = self.baseline_comparator.first_difference
                    self._persist_baseline_diff()
            else:
                self._state = initial_state(scenario.covers)
                self._previous_chain_hash = GENESIS_CHAIN_HASH
                self._write_trace_header()
        else:
            self._documents = []
            self._state = initial_state(scenario.covers)
            self._previous_chain_hash = GENESIS_CHAIN_HASH
            self._write_trace_header()
        self._validate_quantized_cover_values()

    @property
    def covers(self) -> tuple[CoverSpec, ...]:
        return self.scenario.covers

    @property
    def state(self) -> ReplayState:
        with self._lock:
            return copy.deepcopy(self._state)

    @property
    def next_step(self) -> int:
        with self._lock:
            return self._state.step + 1

    @property
    def completed(self) -> bool:
        return self.next_step >= self.config.physical_steps

    @property
    def baseline_diff(self) -> dict[str, Any] | None:
        return self._baseline_diff

    def _prepare_output(self, *, resume: bool, clean: bool) -> None:
        source_dir = self.original_scenario.source_dir.resolve()
        if clean and (
            self.output_dir == source_dir or source_dir.is_relative_to(self.output_dir)
        ):
            raise ReplayError(
                E_REPLAY_INVALID,
                "refusing to clean the scenario source directory",
                None,
                "output",
                {"output": str(self.output_dir), "source": str(source_dir)},
            )
        if self.output_dir.exists() and not self.output_dir.is_dir():
            raise ReplayError(
                E_REPLAY_INVALID,
                "output path exists and is not a directory",
                None,
                "output",
            )
        if clean and self.output_dir.exists():
            shutil.rmtree(self.output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)
        if not resume:
            existing = list(self.output_dir.iterdir())
            if existing:
                names = sorted(path.name for path in existing)[:10]
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "output directory is not empty; use --clean or --resume",
                    None,
                    "output",
                    {"entries": names},
                )
        self.snapshot_dir.mkdir(parents=True, exist_ok=True)

    def _validate_quantized_cover_values(self) -> None:
        for cover in self.covers:
            capacity = _capacity_units(cover, self.config)
            if capacity <= 0:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "max_durability rounds to zero at configured fixed-point precision",
                    cover.record_index,
                    f"covers[{cover.record_index}].max_durability",
                )
            threshold = _threshold_units(cover, self.config)
            if threshold <= 0:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "segment_damage_threshold rounds to zero at configured fixed-point precision",
                    cover.record_index,
                    f"covers[{cover.record_index}].segment_damage_threshold",
                )
            if threshold > capacity:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "segment_damage_threshold exceeds quantized max_durability",
                    cover.record_index,
                    f"covers[{cover.record_index}].segment_damage_threshold",
                    {"threshold_units": threshold, "capacity_units": capacity},
                )

    def _write_trace_header(self) -> None:
        atomic_write_bytes(self.trace_path, b"")

    def _append_trace(self, frame: dict[str, Any]) -> None:
        try:
            with self.trace_path.open("ab") as handle:
                handle.write(canonical_json_bytes(frame) + b"\n")
                handle.flush()
                os.fsync(handle.fileno())
        except OSError as error:
            raise ReplayError(
                E_IO,
                f"failed to append trajectory frame: {error}",
                frame.get("step"),
                str(self.trace_path),
            ) from error

    def cancel(self) -> None:
        self.cancel_event.set()

    def inject_revoke(
        self,
        player_id: str,
        client_seq: int,
        effective_step: int | None = None,
        arrival_seq: int | None = None,
    ) -> Revoke:
        if not isinstance(player_id, str) or not player_id:
            raise ReplayError(
                E_REPLAY_INVALID,
                "player_id must be a non-empty string",
                None,
                "injected_revoke.player_id",
            )
        if isinstance(client_seq, bool) or not isinstance(client_seq, int) or client_seq < 0:
            raise ReplayError(
                E_REPLAY_INVALID,
                "client_seq must be a non-negative integer",
                None,
                "injected_revoke.client_seq",
            )
        shot_key = f"{player_id}:{client_seq}"
        if shot_key not in self.scenario.shot_by_key:
            raise ReplayError(
                E_REVOKE_NOT_FOUND,
                "injected revoke target does not exist",
                None,
                "injected_revoke.client_seq",
                {"shot_key": shot_key},
            )
        with self._lock:
            minimum_step = self._state.step + 1
            if self._processing_step is not None:
                minimum_step = max(minimum_step, self._processing_step + 1)
            selected_step = minimum_step if effective_step is None else effective_step
            if not isinstance(selected_step, int) or selected_step < minimum_step:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "injected revoke effective_step is already committed or in progress",
                    None,
                    "injected_revoke.effective_step",
                    {
                        "actual": effective_step,
                        "minimum": minimum_step,
                    },
                )
            if selected_step >= self.config.physical_steps:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "injected revoke effective_step exceeds physical_steps",
                    None,
                    "injected_revoke.effective_step",
                )
            if arrival_seq is None:
                arrivals = [
                    shot.arrival_seq for shot in self.scenario.shots
                ] + [revoke.arrival_seq for revoke in self.scenario.revokes]
                arrivals += [revoke.arrival_seq for revoke in self._injected_revokes]
                selected_arrival = max(arrivals, default=0) + 1
            else:
                if (
                    isinstance(arrival_seq, bool)
                    or not isinstance(arrival_seq, int)
                    or arrival_seq < 0
                ):
                    raise ReplayError(
                        E_REPLAY_INVALID,
                        "arrival_seq must be a non-negative integer",
                        None,
                        "injected_revoke.arrival_seq",
                    )
                selected_arrival = arrival_seq
            self._injected_counter += 1
            revoke = Revoke(
                index=1_000_000 + self._injected_counter,
                player_id=player_id,
                client_seq=client_seq,
                arrival_seq=selected_arrival,
                effective_step=selected_step,
                source="injected",
            )
            self._injected_revokes.append(revoke)
            return revoke

    def step_once(self) -> dict[str, Any]:
        with self._lock:
            step = self._state.step + 1
            if step >= self.config.physical_steps:
                raise ReplayError(
                    E_REPLAY_INVALID,
                    "all physical steps have already been replayed",
                    None,
                    "config.physical_steps",
                )
            if self.cancel_event.is_set():
                raise ReplayError(E_CANCELLED, "replay cancelled before step start")
            staged = copy.deepcopy(self._state)
            staged.step = step
            self._processing_step = step
            injected_at_start = list(self._injected_revokes)
            try:
                frame = self._settle_step(
                    staged,
                    step,
                    injected_at_start,
                )
                self._state = staged
                self._previous_chain_hash = staged.chain_hash
                self._documents.append(self._last_snapshot_document)
                self._processing_step = None
            except Exception:
                self._processing_step = None
                raise
            if self.crash_after_step is not None and step >= self.crash_after_step:
                os._exit(137)
            if self.baseline_comparator is not None:
                difference = self.baseline_comparator.compare(frame)
                if difference is not None:
                    self._baseline_diff = difference
                    self._persist_baseline_diff()
                self._write_baseline_report()
            return frame

    def _settle_step(
        self,
        staged: ReplayState,
        step: int,
        injected_revokes: list[Revoke],
    ) -> dict[str, Any]:
        step_shots = tuple(
            shot for shot in self.scenario.shots if shot.effective_step == step
        )
        step_revokes = [
            revoke for revoke in self.scenario.revokes if revoke.effective_step == step
        ]
        step_revokes.extend(
            revoke
            for revoke in injected_revokes
            if revoke.effective_step == step
        )
        try:
            damage_by_cover, budget_usage = run_damage_schedule(
                self.covers,
                step_shots,
                self.config,
                self.cancel_event,
            )
        except ReplayError as error:
            self._write_failure(error, step)
            raise
        if self.cancel_event.is_set():
            error = ReplayError(E_CANCELLED, "replay cancelled before publication")
            self._write_failure(error, step)
            raise error

        actions: list[tuple[int, int, str, int, Any]] = []
        for shot in step_shots:
            actions.append(
                (
                    shot.arrival_seq,
                    0,
                    shot.player_id,
                    shot.client_seq,
                    shot,
                )
            )
        for revoke in step_revokes:
            actions.append(
                (
                    revoke.arrival_seq,
                    1,
                    revoke.player_id,
                    revoke.client_seq,
                    revoke,
                )
            )
        actions.sort(key=lambda item: (item[0], item[1], item[2], item[3]))

        revocation_records: list[dict[str, Any]] = []
        for _, action_kind, _, _, action in actions:
            if action_kind == 0:
                shot = action
                predicted_units = damage_by_cover[shot.cover_id][shot.key]
                if shot.key in staged.ledger:
                    error = ReplayError(
                        E_REPLAY_INVALID,
                        "shot ledger key already exists",
                        shot.index,
                        f"shots[{shot.index}].client_seq",
                        {"shot_key": shot.key},
                    )
                    self._write_failure(error, step)
                    raise error
                staged.ledger[shot.key] = LedgerEntry(
                    shot_key=shot.key,
                    player_id=shot.player_id,
                    client_seq=shot.client_seq,
                    cover_id=shot.cover_id,
                    effective_step=shot.effective_step,
                    arrival_seq=shot.arrival_seq,
                    predicted_units=predicted_units,
                    source_index=shot.index,
                )
                staged.covers[shot.cover_id].predicted_damage_units += predicted_units
            else:
                revoke = action
                self._apply_revoke(staged, step, revoke, revocation_records)

        settled_in_step = self._settle_pending_shots(staged, step)
        crack_events = self._resolve_cracks(staged, step)
        staged.buffer_watermark_bytes = max(
            staged.buffer_watermark_bytes,
            int(budget_usage["peak_memory_bytes"]),
        )
        staged.last_schedule = budget_usage
        staged.last_crack_events = crack_events
        staged.last_predicted_ledger = staged.display_ledger(
            self.covers,
            self.config.fixed_point_decimals,
            self.config.quantization_mode,
        )
        staged.revocation_log.extend(revocation_records)
        future_injected = [
            revoke
            for revoke in self._injected_revokes
            if revoke.effective_step > step
        ]
        staged.pending_injected_revokes = future_injected

        state_payload = staged.to_payload()
        state_hash = compute_state_hash(self.fingerprint, state_payload)
        chain_hash = compute_chain_hash(self._previous_chain_hash, state_hash)
        staged.state_hash = state_hash
        staged.chain_hash = chain_hash
        frame = self._build_frame(
            staged,
            step,
            settled_in_step,
            crack_events,
            budget_usage,
            revocation_records,
            chain_hash,
        )
        document = make_snapshot_document(
            self.fingerprint,
            staged,
            frame,
            self._previous_chain_hash,
        )
        if document["state_hash"] != state_hash or document["chain_hash"] != chain_hash:
            raise ReplayError(
                E_IO,
                "internal state hash changed during snapshot construction",
                step,
                "snapshot.state_hash",
            )
        document["frame"] = frame
        write_snapshot(self.snapshot_dir, document)
        self._append_trace(frame)
        self._last_snapshot_document = document
        return frame

    def _apply_revoke(
        self,
        staged: ReplayState,
        step: int,
        revoke: Revoke,
        records: list[dict[str, Any]],
    ) -> None:
        entry = staged.ledger.get(revoke.key)
        if entry is None:
            error = ReplayError(
                E_REVOKE_NOT_FOUND,
                "revoke target is not present in the prediction ledger",
                revoke.index,
                "revokes.client_seq",
                {
                    "shot_key": revoke.key,
                    "effective_step": step,
                    "arrival_seq": revoke.arrival_seq,
                },
            )
            self._write_failure(error, step)
            raise error
        cover_state = staged.covers[entry.cover_id]
        if entry.status == "pending":
            entry.status = "revoked"
            cover_state.predicted_damage_units -= entry.predicted_units
            records.append(
                {
                    "action": "cancelled_pending",
                    "shot_key": entry.shot_key,
                    "cover_id": entry.cover_id,
                    "step": step,
                    "arrival_seq": revoke.arrival_seq,
                    "restoration_units": entry.predicted_units,
                }
            )
            return
        if entry.status == "settled":
            settled_step = entry.settled_step if entry.settled_step is not None else -1
            if step - settled_step <= self.config.revocation_window_steps:
                entry.status = "compensated"
                cover_state.predicted_damage_units -= entry.predicted_units
                staged.compensations.append(
                    CompensationRecord(
                        shot_key=entry.shot_key,
                        cover_id=entry.cover_id,
                        revoke_step=step,
                        original_settled_step=settled_step,
                        restoration_units=entry.predicted_units,
                        authoritative_unchanged_units=entry.authoritative_units,
                    )
                )
                records.append(
                    {
                        "action": "compensated",
                        "shot_key": entry.shot_key,
                        "cover_id": entry.cover_id,
                        "step": step,
                        "arrival_seq": revoke.arrival_seq,
                        "original_settled_step": settled_step,
                        "restoration_units": entry.predicted_units,
                        "authoritative_unchanged_units": entry.authoritative_units,
                    }
                )
                return
        error = ReplayError(
            E_REVOKE_CLOSED,
            "revoke arrived after the compensation window or target is already closed",
            revoke.index,
            "revokes.effective_step",
            {
                "shot_key": entry.shot_key,
                "cover_id": entry.cover_id,
                "ledger_status": entry.status,
                "settled_step": entry.settled_step,
                "revoke_step": step,
                "revocation_window_steps": self.config.revocation_window_steps,
            },
        )
        self._write_failure(error, step)
        raise error

    def _settle_pending_shots(
        self,
        staged: ReplayState,
        step: int,
    ) -> list[Shot]:
        pending_shots = [
            shot
            for shot in self.scenario.shots
            if shot.effective_step == step
            and staged.ledger[shot.key].status == "pending"
        ]
        pending_shots.sort(
            key=lambda shot: (
                shot.player_id,
                shot.client_seq,
                shot.arrival_seq,
                shot.index,
            )
        )
        settled: list[Shot] = []
        for shot in pending_shots:
            entry = staged.ledger[shot.key]
            cover = self.scenario.cover_by_id[shot.cover_id]
            cover_state = staged.covers[shot.cover_id]
            capacity = _capacity_units(cover, self.config)
            authoritative_before = max(0, capacity - cover_state.damage_units)
            authoritative_after = max(0, authoritative_before - entry.predicted_units)
            if authoritative_before == 0:
                entry.status = "returned"
                entry.settled_step = step
                entry.authoritative_units = 0
                cover_state.predicted_damage_units -= entry.predicted_units
                entry.predicted_after_units = (
                    capacity - cover_state.predicted_damage_units
                )
                entry.authoritative_after_units = 0
                staged.settled_shot_keys.append(shot.key)
                settled.append(shot)
                continue
            if entry.predicted_units > authoritative_before:
                predicted_after = authoritative_before - entry.predicted_units
                error = ReplayError(
                    E_CREDIT_CONFLICT,
                    "client prediction exceeds the authoritative remaining durability",
                    shot.index,
                    f"shots[{shot.index}].momentum",
                    {
                        "cover_id": shot.cover_id,
                        "shot_key": shot.key,
                        "shot_index": shot.index,
                        "predicted_durability": format_units(
                            predicted_after,
                            self.config.fixed_point_decimals,
                        ),
                        "authoritative_durability": format_units(
                            authoritative_after,
                            self.config.fixed_point_decimals,
                        ),
                        "predicted_units": entry.predicted_units,
                        "authoritative_before_units": authoritative_before,
                    },
                )
                self._write_failure(error, step)
                raise error
            cover_state.damage_units += entry.predicted_units
            cover_state.direct_damage_units += entry.predicted_units
            cover_state.quantization_operations += 1
            entry.status = "settled"
            entry.settled_step = step
            entry.authoritative_units = entry.predicted_units
            entry.predicted_after_units = capacity - cover_state.predicted_damage_units
            entry.authoritative_after_units = (
                capacity - cover_state.damage_units
            )
            staged.settled_shot_keys.append(shot.key)
            settled.append(shot)
        return settled

    def _resolve_cracks(
        self,
        staged: ReplayState,
        step: int,
    ) -> list[dict[str, Any]]:
        threshold_by_cover = {
            cover.cover_id: _threshold_units(cover, self.config) for cover in self.covers
        }
        pending: dict[str, set[tuple[str, ...]]] = {}
        for cover in self.covers:
            cover_state = staged.covers[cover.cover_id]
            target_segments = cover_state.damage_units // threshold_by_cover[cover.cover_id]
            if target_segments > cover_state.crack_segments:
                pending[cover.cover_id] = {(cover.cover_id,)}
        events: list[dict[str, Any]] = []
        iterations = 0
        cover_order = {cover.cover_id: cover.record_index for cover in self.covers}
        while pending:
            if iterations >= self.config.crack_max_iterations:
                unstable_covers = sorted(pending, key=lambda item: cover_order[item])
                paths: list[list[str]] = []
                for cover_id in unstable_covers:
                    for path in sorted(pending[cover_id]):
                        paths.append(list(path))
                error = ReplayError(
                    E_CRACK_UNSTABLE,
                    "crack propagation did not stabilise within the declared iteration limit",
                    None,
                    "config.crack_max_iterations",
                    {
                        "step": step,
                        "max_iterations": self.config.crack_max_iterations,
                        "unstable_covers": unstable_covers,
                        "unstable_paths": paths[:128],
                        "paths_truncated": len(paths) > 128,
                    },
                )
                self._write_failure(error, step)
                raise error
            wave = pending
            pending = {}
            for cover_id in sorted(wave, key=lambda item: cover_order[item]):
                cover = self.scenario.cover_by_id[cover_id]
                cover_state = staged.covers[cover_id]
                threshold = threshold_by_cover[cover_id]
                target_segments = cover_state.damage_units // threshold
                paths = sorted(wave[cover_id])
                while cover_state.crack_segments < target_segments:
                    cover_state.crack_segments += 1
                    path = paths[0] if paths else (cover_id,)
                    source_cover_id = path[-2] if len(path) > 1 else cover_id
                    propagation_kind = "primary" if len(path) == 1 else "chain"
                    events.append(
                        {
                            "step": step,
                            "cover_id": cover_id,
                            "segment_index": cover_state.crack_segments,
                            "source_cover_id": source_cover_id,
                            "path": list(path),
                            "kind": propagation_kind,
                            "damage_units": cover_state.damage_units,
                            "damage": format_units(
                                cover_state.damage_units,
                                self.config.fixed_point_decimals,
                            ),
                            "threshold": format_units(
                                threshold,
                                self.config.fixed_point_decimals,
                            ),
                        }
                    )
                    for neighbor in cover.neighbors:
                        receive_units = propagation_units(cover, neighbor, self.config)
                        if receive_units <= 0:
                            continue
                        neighbor_state = staged.covers[neighbor.cover_id]
                        neighbor_capacity = _capacity_units(
                            self.scenario.cover_by_id[neighbor.cover_id],
                            self.config,
                        )
                        new_damage = min(
                            neighbor_capacity,
                            neighbor_state.damage_units + receive_units,
                        )
                        applied_units = new_damage - neighbor_state.damage_units
                        if applied_units <= 0:
                            continue
                        neighbor_state.damage_units = new_damage
                        neighbor_state.propagated_damage_units += applied_units
                        neighbor_state.quantization_operations += 1
                        new_path = path + (neighbor.cover_id,)
                        pending.setdefault(neighbor.cover_id, set()).add(new_path)
            iterations += 1
            stable_pending: dict[str, set[tuple[str, ...]]] = {}
            for cover_id, paths in pending.items():
                cover_state = staged.covers[cover_id]
                target_segments = (
                    cover_state.damage_units // threshold_by_cover[cover_id]
                )
                if target_segments > cover_state.crack_segments:
                    stable_pending[cover_id] = paths
            pending = stable_pending
        for cover in self.covers:
            cover_state = staged.covers[cover.cover_id]
            capacity = _capacity_units(cover, self.config)
            cover_state.damage_units = min(cover_state.damage_units, capacity)
        return events

    def _build_frame(
        self,
        state: ReplayState,
        step: int,
        settled_shots: list[Shot],
        crack_events: list[dict[str, Any]],
        budget_usage: dict[str, Any],
        revocations: list[dict[str, Any]],
        chain_hash: str,
    ) -> dict[str, Any]:
        decimals = self.config.fixed_point_decimals
        covers = []
        for cover in self.covers:
            cover_state = state.covers[cover.cover_id]
            capacity = _capacity_units(cover, self.config)
            covers.append(
                {
                    "cover_id": cover.cover_id,
                    "accumulated_damage": format_units(
                        cover_state.damage_units,
                        decimals,
                    ),
                    "remaining_durability": format_units(
                        max(0, capacity - cover_state.damage_units),
                        decimals,
                    ),
                    "predicted_remaining": format_units(
                        capacity - cover_state.predicted_damage_units,
                        decimals,
                    ),
                    "cracked": cover_state.crack_segments > 0,
                    "crack_segments": cover_state.crack_segments,
                    "quantization_error_bound": self._error_bound(
                        cover_state.quantization_operations
                    ),
                }
            )
        settled_shots = sorted(
            settled_shots,
            key=lambda shot: (
                shot.player_id,
                shot.client_seq,
                shot.arrival_seq,
                shot.index,
            ),
        )
        return {
            "step": step,
            "effective_shots": [
                {
                    "player_id": shot.player_id,
                    "client_seq": shot.client_seq,
                    "cover_id": shot.cover_id,
                    "arrival_seq": shot.arrival_seq,
                }
                for shot in settled_shots
            ],
            "effective_shot_client_sequence": [shot.client_seq for shot in settled_shots],
            "state_hash": state.state_hash,
            "chain_hash": chain_hash,
            "covers": covers,
            "crack_events": crack_events,
            "budget_usage": budget_usage,
            "buffer_watermark_bytes": state.buffer_watermark_bytes,
            "step_peak_memory_bytes": budget_usage["peak_memory_bytes"],
            "predicted_ledger": state.last_predicted_ledger,
            "revocation_events": revocations,
            "compensations": [
                record.to_dict()
                for record in state.compensations
                if record.revoke_step == step
            ],
        }

    def _error_bound(self, operations: int) -> str:
        from .fixed import quantization_error_bound

        return quantization_error_bound(operations, self.config.fixed_point_decimals)

    def _write_failure(self, error: ReplayError, step: int) -> None:
        try:
            self.output_dir.mkdir(parents=True, exist_ok=True)
            atomic_write_bytes(
                self.failure_path,
                canonical_json_bytes(
                    {
                        "step": step,
                        "error": error.to_dict(),
                    }
                )
                + b"\n",
            )
        except ReplayError:
            pass

    def _write_baseline_report(self) -> None:
        if self.baseline_comparator is None:
            return
        atomic_write_bytes(
            self.baseline_report_path,
            canonical_json_bytes(self.baseline_comparator.report()) + b"\n",
        )

    def _persist_baseline_diff(self) -> None:
        if self._baseline_diff is None:
            self.baseline_diff_path.unlink(missing_ok=True)
            return
        atomic_write_bytes(
            self.baseline_diff_path,
            canonical_json_bytes(
                {
                    "code": E_BASELINE_MISMATCH,
                    "first_difference": self._baseline_diff,
                }
            )
            + b"\n",
        )

    def run(self, max_steps: int | None = None) -> ReplayResult:
        self.failure_path.unlink(missing_ok=True)
        frames: list[dict[str, Any]] = []
        status = "completed"
        limit = self.config.physical_steps
        if max_steps is not None:
            limit = min(limit, self._state.step + 1 + max_steps)
        while self._state.step + 1 < limit:
            if self.cancel_event.is_set():
                status = "cancelled"
                break
            try:
                frames.append(self.step_once())
            except ReplayError as error:
                if error.code == E_CANCELLED:
                    status = "cancelled"
                    break
                raise
        if self._baseline_diff is not None:
            status = "baseline_mismatch"
        elif self._state.step + 1 < self.config.physical_steps:
            status = "paused" if status == "completed" else status
        self._write_baseline_report()
        return ReplayResult(
            status=status,
            steps_completed=max(0, self._state.step + 1),
            state=copy.deepcopy(self._state),
            trace_frames=frames,
            baseline_diff=self._baseline_diff,
        )


def run_offline(
    scenario: Scenario,
    *,
    decimals: int | None = None,
    cancel_event: threading.Event | None = None,
) -> list[dict[str, Any]]:
    import tempfile

    with tempfile.TemporaryDirectory(prefix="cover-replay-offline-") as temporary:
        engine = ReplayEngine(
            scenario,
            Path(temporary) / "out",
            compare_baseline=False,
            decimals_override=decimals,
            cancel_event=cancel_event,
        )
        return engine.run().trace_frames


def compute_quantization_impact(
    scenario: Scenario,
    reference_decimals: int,
    selected_decimals: int,
) -> dict[str, Any]:
    reference_frames = run_offline(scenario, decimals=reference_decimals)
    selected_frames = run_offline(scenario, decimals=selected_decimals)
    reference_by_step = {frame["step"]: frame for frame in reference_frames}
    selected_by_step = {frame["step"]: frame for frame in selected_frames}
    affected: list[dict[str, Any]] = []
    for step in sorted(set(reference_by_step) & set(selected_by_step)):
        reference_covers = {
            cover["cover_id"]: cover for cover in reference_by_step[step]["covers"]
        }
        selected_covers = {
            cover["cover_id"]: cover for cover in selected_by_step[step]["covers"]
        }
        for cover in scenario.covers:
            reference = reference_covers[cover.cover_id]
            selected = selected_covers[cover.cover_id]
            if reference["crack_segments"] == selected["crack_segments"]:
                continue
            if not reference["cracked"] and selected["cracked"]:
                reason = "selected_precision_crossed_threshold"
            elif reference["cracked"] and not selected["cracked"]:
                reason = "selected_precision_avoided_threshold"
            else:
                reason = "selected_precision_changed_segment_count"
            affected.append(
                {
                    "step": step,
                    "cover_id": cover.cover_id,
                    "reason": reason,
                    "reference_decimals": reference_decimals,
                    "selected_decimals": selected_decimals,
                    "reference_accumulated_damage": reference["accumulated_damage"],
                    "selected_accumulated_damage": selected["accumulated_damage"],
                    "reference_threshold": format_units(
                        quantize_units(
                            cover.segment_damage_threshold,
                            reference_decimals,
                            scenario.config.quantization_mode,
                        ),
                        reference_decimals,
                    ),
                    "selected_threshold": format_units(
                        quantize_units(
                            cover.segment_damage_threshold,
                            selected_decimals,
                            scenario.config.quantization_mode,
                        ),
                        selected_decimals,
                    ),
                    "reference_crack_segments": reference["crack_segments"],
                    "selected_crack_segments": selected["crack_segments"],
                }
            )
    return {
        "reference_decimals": reference_decimals,
        "selected_decimals": selected_decimals,
        "affected_cover_count": len(affected),
        "affected_covers": affected,
    }

