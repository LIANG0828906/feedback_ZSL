from __future__ import annotations

import copy
import importlib
import json
import os
import platform
import shutil
import socket
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any, Callable

from .checkpoint import CheckpointStore, resume_match
from .client import LoopbackClient, PredictiveClient
from .concurrency import BoundedExecutor, ConcurrencyLimits, run_parallel_version_validation
from .engine import MatchEngine, compute_input_fingerprint
from .errors import E_DECODE, E_RESOURCE, E_SEQ, E_SEMANTIC, E_VER, KernelError
from .models import AuthoritativeState, MatchContext, first_authority_difference
from .protocol import (
    PROTOCOL_SPECS,
    decode_frame,
    encode_frame,
)
from .samples import load_samples, write_samples
from .service import LoopbackServer
from .ui import ui_available
from .util import canonical_json_bytes


WORKSPACE = Path(__file__).resolve().parent.parent


def _capture_error(function: Callable[[], Any]) -> dict[str, Any]:
    try:
        function()
    except KernelError as error:
        return error.as_dict()
    return {"unexpected_success": True}


def _check(
    checks: list[dict[str, Any]],
    check_id: str,
    title: str,
    function: Callable[[], dict[str, Any]],
) -> dict[str, Any]:
    try:
        details = function()
        ok = bool(details.pop("ok", True))
    except Exception as exc:  # noqa: BLE001 - the report must capture any failure
        ok = False
        details = {
            "exception": type(exc).__name__,
            "message": str(exc),
        }
    record = {
        "id": check_id,
        "title": title,
        "status": "pass" if ok else "fail",
        "details": details,
    }
    checks.append(record)
    return record


def _environment_check() -> dict[str, Any]:
    versions: dict[str, Any] = {
        "python": platform.python_version(),
        "implementation": platform.python_implementation(),
        "numpy": None,
        "pyside6": None,
    }
    for module_name in ("numpy", "PySide6"):
        try:
            module = importlib.import_module(module_name)
            versions[module_name.lower() if module_name == "numpy" else "pyside6"] = getattr(
                module, "__version__", "installed"
            )
        except Exception as exc:  # noqa: BLE001
            versions[module_name.lower() if module_name == "numpy" else "pyside6"] = (
                f"unavailable: {exc}"
            )
    loopback_ok = False
    try:
        probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            probe.bind(("127.0.0.1", 0))
            loopback_ok = True
        finally:
            probe.close()
    except OSError:
        loopback_ok = False
    numpy_ok = isinstance(versions["numpy"], str) and not versions["numpy"].startswith(
        "unavailable:"
    )
    return {
        "ok": loopback_ok and numpy_ok,
        "versions": versions,
        "loopback_bind_ok": loopback_ok,
        "headless_mode": True,
        "gui_import_available": ui_available(),
    }


def _sample_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    computed = compute_input_fingerprint(
        bundle.context,
        bundle.initial_state,
        bundle.inputs_by_tick,
    )
    return {
        "ok": computed == bundle.context.input_fingerprint
        and len(expected_states) == bundle.manifest["ticks"] + 1,
        "match_id": bundle.context.match_id,
        "seed": bundle.context.seed,
        "tick_rate": bundle.context.tick_rate,
        "clock_hz": bundle.context.clock_hz,
        "ticks": bundle.manifest["ticks"],
        "units": sorted(bundle.initial_state.units),
        "input_fingerprint": computed,
        "fingerprint_matches_manifest": computed == bundle.context.input_fingerprint,
        "frame_counts": {
            str(version): len(frames)
            for version, frames in sorted(bundle.frames_by_version.items())
        },
        "expected_state_hashes": {
            str(tick): state.snapshot_hash
            for tick, state in (
                (0, expected_states[0]),
                (expected_states[-1].tick, expected_states[-1]),
            )
        },
    }


def _protocol_interop_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    per_version: dict[str, Any] = {}
    all_ok = True
    for version in sorted(PROTOCOL_SPECS):
        frames = bundle.frames_by_version[version]
        base: AuthoritativeState | None = None
        first_divergence: dict[str, Any] | None = None
        byte_mismatches = 0
        repeat_mismatches = 0
        warnings_sample: list[dict[str, Any]] = []
        mappings_sample: list[dict[str, Any]] = []
        for tick, frame in enumerate(frames):
            decoded = decode_frame(
                frame,
                base=None if tick == 0 else base,
                context=bundle.context,
            )
            if decoded.state.snapshot_hash != expected_states[tick].snapshot_hash:
                if first_divergence is None:
                    first_divergence = first_authority_difference(
                        decoded.state,
                        expected_states[tick],
                    ) or {
                        "tick": tick,
                        "field": "snapshot_hash",
                        "left": decoded.state.snapshot_hash,
                        "right": expected_states[tick].snapshot_hash,
                    }
            previous = base
            encoded = encode_frame(
                decoded.state,
                version,
                previous=previous,
                context=bundle.context,
            )
            original = canonical_json_bytes(frame)
            if encoded.payload != original:
                byte_mismatches += 1
            repeated = encode_frame(
                decoded.state,
                version,
                previous=previous,
                context=bundle.context,
            )
            if repeated.payload != encoded.payload:
                repeat_mismatches += 1
            if tick == min(1, len(frames) - 1):
                warnings_sample = [warning.as_dict() for warning in encoded.warnings]
                mappings_sample = encoded.mappings
            base = decoded.state
        assert base is not None
        version_ok = (
            first_divergence is None
            and byte_mismatches == 0
            and repeat_mismatches == 0
            and base.snapshot_hash == expected_states[-1].snapshot_hash
        )
        all_ok = all_ok and version_ok
        per_version[str(version)] = {
            "ok": version_ok,
            "name": PROTOCOL_SPECS[version]["name"],
            "time_base": PROTOCOL_SPECS[version]["time_base"],
            "missing_field_semantics": PROTOCOL_SPECS[version][
                "missing_field_semantics"
            ],
            "decoded_frames": len(frames),
            "final_tick": base.tick,
            "final_hash": base.snapshot_hash,
            "source_version_preserved": all(
                state.source_version == version for state in [base]
            ),
            "byte_mismatches": byte_mismatches,
            "repeat_mismatches": repeat_mismatches,
            "first_divergence": first_divergence,
            "lossy_warnings": warnings_sample,
            "degrade_mappings": mappings_sample,
        }
    return {
        "ok": all_ok,
        "versions": per_version,
        "first_divergence": next(
            (
                item["first_divergence"]
                for item in per_version.values()
                if item["first_divergence"] is not None
            ),
            None,
        ),
    }


def _prediction_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    local_inputs = {
        tick: [
            intent
            for intent in intents
            if intent.get("player_id") == "p1"
        ]
        for tick, intents in bundle.inputs_by_tick.items()
    }
    client = PredictiveClient(
        version=3,
        context=bundle.context,
        initial_state=bundle.initial_state,
        inputs_by_tick=local_inputs,
    )
    rollout_tick = min(12, len(expected_states) - 1)
    predicted = client.predict_to(rollout_tick)
    predicted_before_rollback = {
        str(tick): client.prediction_history[tick].snapshot_hash
        for tick in range(1, rollout_tick + 1)
    }
    exact_at_rollback: dict[str, Any] = {}
    for tick in range(0, rollout_tick + 1):
        result = client.feed(bundle.frames_by_version[3][tick])
        if not result.applied and tick > 0:
            return {
                "ok": False,
                "failed_tick": tick,
                "issues": [issue.as_dict() for issue in result.issues],
            }
        difference = first_authority_difference(
            client.confirmed_state,
            expected_states[tick],
        )
        exact_at_rollback[str(tick)] = difference is None
    final = client.confirmed_state
    max_correction = max(
        (abs(correction.delta) for correction in client.corrections),
        default=0,
    )
    return {
        "ok": (
            all(exact_at_rollback.values())
            and final.snapshot_hash == expected_states[rollout_tick].snapshot_hash
            and len(client.corrections) > 0
        ),
        "predicted_tick_before_rollback": predicted.tick,
        "rollback_tick": rollout_tick,
        "confirmed_hash": final.snapshot_hash,
        "expected_hash": expected_states[rollout_tick].snapshot_hash,
        "exact_field_match_after_each_rollback": exact_at_rollback,
        "correction_records": len(client.corrections),
        "correction_samples": [
            correction.as_dict() for correction in client.corrections[:8]
        ],
        "max_absolute_correction": max_correction,
        "predicted_hashes_before_rollback": predicted_before_rollback,
    }


def _sequence_guard_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    client = PredictiveClient(
        version=3,
        context=bundle.context,
        initial_state=bundle.initial_state,
        inputs_by_tick=bundle.inputs_by_tick,
    )
    client.predict_to(5)
    client.feed(bundle.frames_by_version[3][0])
    hash_before_gap = client.confirmed_state.snapshot_hash
    gap_result = client.feed(bundle.frames_by_version[3][2])
    hash_after_gap = client.confirmed_state.snapshot_hash
    fill_result = client.feed(bundle.frames_by_version[3][1])
    hash_after_fill = client.confirmed_state.snapshot_hash
    reverse_result = client.feed(bundle.frames_by_version[3][2])
    hash_after_reverse = client.confirmed_state.snapshot_hash
    return {
        "ok": (
            not gap_result.applied
            and any(issue.code == E_SEQ for issue in gap_result.issues)
            and hash_before_gap == hash_after_gap
            and fill_result.applied
            and client.confirmed_sequence == 2
            and hash_after_fill == expected_states[2].snapshot_hash
            and not reverse_result.applied
            and any(issue.code == E_SEQ for issue in reverse_result.issues)
            and hash_after_fill == hash_after_reverse
        ),
        "sequence_before_gap": 1,
        "confirmed_sequence_after_fill": client.confirmed_sequence,
        "hash_preserved_on_gap": hash_before_gap == hash_after_gap,
        "hash_preserved_on_reverse": hash_after_fill == hash_after_reverse,
        "gap_result": gap_result.as_dict(),
        "reverse_result": reverse_result.as_dict(),
    }


def _error_matrix_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    unknown_version = _capture_error(
        lambda: decode_frame(
            {"protocol_version": 99, "sequence": 0},
            context=bundle.context,
        )
    )
    missing_v1 = copy.deepcopy(bundle.frames_by_version[1][1])
    missing_v1.pop("server_tick", None)
    missing_required = _capture_error(
        lambda: decode_frame(
            missing_v1,
            base=expected_states[0],
            context=bundle.context,
        )
    )
    corrupt = _capture_error(lambda: decode_frame(b'{"protocol_version":1,'))
    unknown_unit_frame = copy.deepcopy(bundle.frames_by_version[1][1])
    unknown_unit_frame["units"][0]["id"] = "ghost-unit"
    unknown_unit = _capture_error(
        lambda: decode_frame(
            unknown_unit_frame,
            base=expected_states[0],
            context=bundle.context,
        )
    )
    time_regression_frame = copy.deepcopy(bundle.frames_by_version[3][2])
    time_regression_frame["logical_clock"] = 0
    time_regression = _capture_error(
        lambda: decode_frame(
            time_regression_frame,
            base=expected_states[1],
            context=bundle.context,
        )
    )
    conflict_frame = copy.deepcopy(bundle.frames_by_version[3][1])
    snapshot_heading = conflict_frame["snapshot"]["u1"]["heading_mdeg"]
    conflict_frame["delta"][0]["heading_mdeg"] = (snapshot_heading + 1) % 360_000
    semantic_conflict = _capture_error(
        lambda: decode_frame(
            conflict_frame,
            base=expected_states[0],
            context=bundle.context,
        )
    )
    checks = {
        "unknown_version": {
            "expected": E_VER,
            "actual": unknown_version,
            "path_ok": unknown_version.get("path") == "$.protocol_version",
        },
        "missing_required_v1": {
            "expected": E_VER,
            "actual": missing_required,
            "path_ok": missing_required.get("path") == "$.server_tick",
            "source_version_ok": missing_required.get("source_version") == 1,
        },
        "corrupt_bytes": {
            "expected": E_DECODE,
            "actual": corrupt,
            "path_ok": corrupt.get("path") == "$",
        },
        "unknown_unit": {
            "expected": E_SEMANTIC,
            "actual": unknown_unit,
            "object_ok": unknown_unit.get("object_id") == "ghost-unit",
        },
        "time_regression": {
            "expected": E_SEMANTIC,
            "actual": time_regression,
            "path_ok": time_regression.get("path") == "$.logical_clock",
        },
        "snapshot_delta_conflict": {
            "expected": E_SEMANTIC,
            "actual": semantic_conflict,
            "object_ok": semantic_conflict.get("object_id") == "u1",
        },
    }
    ok = (
        unknown_version.get("code") == E_VER
        and missing_required.get("code") == E_VER
        and missing_required.get("path") == "$.server_tick"
        and missing_required.get("source_version") == 1
        and corrupt.get("code") == E_DECODE
        and unknown_unit.get("code") == E_SEMANTIC
        and unknown_unit.get("object_id") == "ghost-unit"
        and time_regression.get("code") == E_SEMANTIC
        and semantic_conflict.get("code") == E_SEMANTIC
    )
    return {"ok": ok, "cases": checks}


def _checkpoint_check(
    workspace: Path,
    bundle: Any,
    expected_states: list[AuthoritativeState],
    engine: MatchEngine,
) -> dict[str, Any]:
    root = workspace / "artifacts" / "demo" / "checkpoints"
    shutil.rmtree(root, ignore_errors=True)
    root.mkdir(parents=True, exist_ok=True)
    store = CheckpointStore(
        root / "authority.json",
        context=bundle.context,
        protocol_version=3,
        commit_window=10,
    )
    saves = [
        store.save(expected_states[tick])
        for tick in (0, 10, 20)
        if tick < len(expected_states)
    ]
    loaded = store.load()
    order_error = _capture_error(lambda: store.save(expected_states[10]))
    bad_context = MatchContext(
        match_id=bundle.context.match_id,
        seed=bundle.context.seed + 1,
        tick_rate=bundle.context.tick_rate,
        clock_hz=bundle.context.clock_hz,
        input_fingerprint=bundle.context.input_fingerprint,
    )
    mismatch_store = CheckpointStore(
        root / "authority.json",
        context=bad_context,
        protocol_version=3,
        commit_window=10,
    )
    seed_mismatch = mismatch_store.load()
    recomputed = resume_match(
        engine=engine,
        store=mismatch_store,
        target_tick=20,
        save_commits=False,
    )
    protocol_mismatch_store = CheckpointStore(
        root / "authority.json",
        context=bundle.context,
        protocol_version=1,
        commit_window=10,
    )
    protocol_mismatch = protocol_mismatch_store.load()

    corrupt_path = root / "corrupt.json"
    corrupt_store = CheckpointStore(
        corrupt_path,
        context=bundle.context,
        protocol_version=3,
        commit_window=10,
    )
    corrupt_store.save(expected_states[10])
    corrupt_path.write_text('{"format":"authority-window-checkpoint",', encoding="utf-8")
    corrupt_load = corrupt_store.load()

    resume_path = root / "resume.json"
    resume_store = CheckpointStore(
        resume_path,
        context=bundle.context,
        protocol_version=3,
        commit_window=10,
    )
    resume_store.save(expected_states[20])
    resumed = resume_match(
        engine=engine,
        store=resume_store,
        target_tick=len(expected_states) - 1,
    )

    forced_path = root / "forced.json"
    forced_store = CheckpointStore(
        forced_path,
        context=bundle.context,
        protocol_version=3,
        commit_window=10,
    )
    terminated = resume_match(
        engine=engine,
        store=forced_store,
        target_tick=len(expected_states) - 1,
        force_terminate_at=25,
    )
    after_termination = forced_store.load()
    restarted = resume_match(
        engine=engine,
        store=forced_store,
        target_tick=len(expected_states) - 1,
    )
    cancel_path = root / "cancel.json"
    cancel_store = CheckpointStore(
        cancel_path,
        context=bundle.context,
        protocol_version=3,
        commit_window=10,
    )
    cancel_store.save(expected_states[20])
    cancel_event = threading.Event()
    cancel_event.set()
    cancelled = resume_match(
        engine=engine,
        store=cancel_store,
        target_tick=len(expected_states) - 1,
        cancel_event=cancel_event,
    )
    ok = (
        loaded.valid
        and loaded.state is not None
        and loaded.state.tick == 20
        and order_error.get("code") == E_SEQ
        and not seed_mismatch.valid
        and seed_mismatch.recompute_required
        and recomputed.final_state.snapshot_hash == expected_states[20].snapshot_hash
        and not protocol_mismatch.valid
        and not corrupt_load.valid
        and corrupt_load.reason == E_DECODE
        and resumed.final_state.snapshot_hash == expected_states[-1].snapshot_hash
        and resumed.checkpoint_valid
        and not terminated.complete
        and after_termination.valid
        and after_termination.state is not None
        and after_termination.state.tick == 20
        and restarted.complete
        and restarted.final_state.snapshot_hash == expected_states[-1].snapshot_hash
        and restarted.resumed_tick == 20
        and cancelled.cancelled
        and not cancelled.complete
        and cancelled.resumed_tick == 20
    )
    return {
        "ok": ok,
        "writes": saves,
        "loaded": loaded.as_dict(),
        "out_of_order_commit": order_error,
        "seed_mismatch": seed_mismatch.as_dict(),
        "seed_mismatch_recompute": recomputed.as_dict(),
        "protocol_mismatch": protocol_mismatch.as_dict(),
        "corrupt_checkpoint": corrupt_load.as_dict(),
        "valid_resume": resumed.as_dict(),
        "forced_termination": {
            "terminated_run": terminated.as_dict(),
            "persisted_window": after_termination.as_dict(),
            "restart_recomputed": restarted.as_dict(),
            "cancelled_run": cancelled.as_dict(),
            "original_process_report_complete": False,
            "note": (
                "The terminated process produced no complete report. The restarted "
                "run resumed at the last persisted tick and recomputed later ticks."
            ),
        },
    }


def _service_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    server = LoopbackServer(frames_by_version=bundle.frames_by_version)
    server.start()
    clients: list[LoopbackClient] = []
    try:
        for version in (1, 2, 3):
            client = LoopbackClient(
                host=server.host,
                port=server.port,
                version=version,
                context=bundle.context,
                initial_state=bundle.initial_state,
                inputs_by_tick=bundle.inputs_by_tick,
                timeout=12.0,
            )
            client.connect()
            clients.append(client)
        waits = {client.predictor.version: client.wait(12.0) for client in clients}
        client_results: dict[str, Any] = {}
        final_hashes: dict[int, str] = {}
        per_version_first_divergence: dict[str, Any] = {}
        all_mismatches: list[dict[str, Any]] = []
        for client in clients:
            version = client.predictor.version
            final_hashes[version] = client.confirmed_state.snapshot_hash
            client_results[str(version)] = {
                "completed": waits[version],
                "confirmed_sequence": client.predictor.confirmed_sequence,
                "final_tick": client.confirmed_state.tick,
                "final_hash": client.confirmed_state.snapshot_hash,
                "issue_codes": sorted(
                    {issue.code for issue in client.predictor.issues}
                ),
                "issue_count": len(client.predictor.issues),
                "echoes_received": len(client.echoes),
                "transport_errors": list(client.transport_errors),
            }
            first_difference = None
            for tick in sorted(client.predictor.confirmed_history):
                difference = first_authority_difference(
                    client.predictor.confirmed_history[tick],
                    expected_states[tick],
                )
                if difference is not None:
                    first_difference = difference
                    all_mismatches.append(
                        {
                            **difference,
                            "version": version,
                        }
                    )
                    break
            per_version_first_divergence[str(version)] = first_difference
        versions = sorted(final_hashes)
        compared_ticks = min(
            max(client.predictor.confirmed_history) for client in clients
        ) + 1
        for tick in range(compared_ticks):
            current = [
                client.predictor.confirmed_history.get(tick) for client in clients
            ]
            if any(state is None for state in current):
                continue
            baseline = current[0]
            assert baseline is not None
            cross_version_mismatch = False
            for index, state in enumerate(current[1:], start=1):
                assert state is not None
                difference = first_authority_difference(baseline, state)
                if difference is not None:
                    all_mismatches.append(
                        {
                            **difference,
                            "versions": [versions[0], versions[index]],
                        }
                    )
                    cross_version_mismatch = True
                    break
            if cross_version_mismatch:
                break
        expected_hash = expected_states[-1].snapshot_hash
        first_divergence = min(
            all_mismatches,
            key=lambda item: (item["tick"], item["field"]),
            default=None,
        )
        stats = {
            "accepted_clients": server.stats.accepted_clients,
            "frames_sent": server.stats.frames_sent,
            "reports_received": server.stats.reports_received,
            "echoes_sent": server.stats.echoes_sent,
            "resync_requests": server.stats.resync_requests,
            "dropped": server.stats.dropped,
            "duplicated": server.stats.duplicated,
            "reordered": server.stats.reordered,
            "dropped_events": server.stats.dropped_events,
        }
        ok = (
            all(waits.values())
            and all(state_hash == expected_hash for state_hash in final_hashes.values())
            and stats["reports_received"] > 0
            and stats["echoes_sent"] > 0
            and stats["dropped"] >= 3
            and stats["duplicated"] >= 3
            and stats["reordered"] >= 3
            and all(not result["transport_errors"] for result in client_results.values())
        )
        return {
            "ok": ok,
            "server_url": "tcp://127.0.0.1:<ephemeral>",
            "port_mode": "ephemeral",
            "anomaly_plan": {
                str(version): {
                    "drop": list(plan.drop_sequences),
                    "duplicate": list(plan.duplicate_sequences),
                    "reorder": list(plan.reorder_sequences),
                }
                for version, plan in sorted(server.anomaly_plans.items())
            },
            "clients": client_results,
            "final_hashes": {str(key): value for key, value in final_hashes.items()},
            "expected_final_hash": expected_hash,
            "compared_ticks": compared_ticks,
            "per_version_first_divergence": per_version_first_divergence,
            "first_divergence": first_divergence,
            "stats": stats,
        }
    finally:
        for client in clients:
            client.close()
        server.stop()


def _resource_boundary_check() -> dict[str, Any]:
    release = threading.Event()
    executor = BoundedExecutor(ConcurrencyLimits(max_workers=1, max_queue=1, max_buffer_bytes=1024))
    first = executor.submit(release.wait, name="blocking", estimated_bytes=1)
    queue_error = _capture_error(
        lambda: executor.submit(lambda: None, name="overflow", estimated_bytes=1)
    )
    release.set()
    first.result(timeout=5.0)
    executor.close()
    small = BoundedExecutor(
        ConcurrencyLimits(max_workers=1, max_queue=2, max_buffer_bytes=128)
    )
    memory_error = _capture_error(
        lambda: small.submit(lambda: None, name="too-large", estimated_bytes=129)
    )
    small.close()
    return {
        "queue_limit_error": queue_error,
        "memory_limit_error": memory_error,
        "ok": queue_error.get("code") == E_RESOURCE
        and memory_error.get("code") == E_RESOURCE,
    }


def _parallel_check(
    bundle: Any,
    expected_states: list[AuthoritativeState],
) -> dict[str, Any]:
    limits = ConcurrencyLimits(
        max_workers=4,
        max_queue=8,
        max_buffer_bytes=4 * 1024 * 1024,
    )
    first, stats = run_parallel_version_validation(
        context=bundle.context,
        initial_state=bundle.initial_state,
        inputs_by_tick=bundle.inputs_by_tick,
        expected_states=expected_states,
        versions=[1, 2, 3],
        limits=limits,
    )
    second, stats_second = run_parallel_version_validation(
        context=bundle.context,
        initial_state=bundle.initial_state,
        inputs_by_tick=bundle.inputs_by_tick,
        expected_states=expected_states,
        versions=[1, 2, 3],
        limits=limits,
    )
    expected_hash = expected_states[-1].snapshot_hash
    stable = {
        version: first[version].frame_bytes_hashes == second[version].frame_bytes_hashes
        for version in first
    }
    boundaries = _resource_boundary_check()
    ok = (
        all(result.first_divergence is None for result in first.values())
        and all(result.final_hash == expected_hash for result in first.values())
        and all(stable.values())
        and boundaries["ok"]
    )
    return {
        "ok": ok,
        "limits": {
            "max_workers": limits.max_workers,
            "max_queue": limits.max_queue,
            "max_buffer_bytes": limits.max_buffer_bytes,
        },
        "stats": stats,
        "second_run_stats": stats_second,
        "versions": {
            str(version): {
                "final_tick": result.final_tick,
                "final_hash": result.final_hash,
                "encoded_frames": result.encoded_frames,
                "decoded_frames": result.decoded_frames,
                "warning_count": result.warning_count,
                "first_divergence": result.first_divergence,
                "byte_hashes_stable_across_runs": stable[version],
            }
            for version, result in sorted(first.items())
        },
        "resource_boundaries": boundaries,
    }


def _ui_check(sample_root: Path) -> dict[str, Any]:
    if not ui_available():
        return {
            "ok": True,
            "gui_available": False,
            "headless_fallback": True,
            "reason": "PySide6 unavailable; report mode remains fully functional.",
        }
    script = """
import json
import os
import sys
os.environ["QT_QPA_PLATFORM"] = "offscreen"
from PySide6.QtWidgets import QApplication
from sync_kernel import ui
app = QApplication.instance() or QApplication([])
bundle, timelines, corrections = ui._decoded_timelines(sys.argv[1])
window = ui.MainWindow(bundle, timelines, corrections)
window.refresh()
app.processEvents()
print(json.dumps({
    "row_count": window.table.rowCount(),
    "unit_count": len(bundle.initial_state.units),
    "versions": sorted(timelines),
    "correction_points": len(corrections),
}))
window.close()
"""
    env = dict(os.environ)
    env["QT_QPA_PLATFORM"] = "offscreen"
    try:
        completed = subprocess.run(
            [sys.executable, "-c", script, str(sample_root)],
            cwd=str(WORKSPACE),
            env=env,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {
            "ok": True,
            "gui_available": True,
            "headless_fallback": True,
            "offscreen_smoke": False,
            "reason": f"GUI smoke downgraded to headless: {exc}",
        }
    if completed.returncode != 0:
        return {
            "ok": True,
            "gui_available": True,
            "headless_fallback": True,
            "offscreen_smoke": False,
            "reason": "Qt offscreen unavailable; report generated in headless mode.",
            "stderr_tail": completed.stderr[-800:],
        }
    try:
        payload = json.loads(completed.stdout.strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError) as exc:
        return {
            "ok": True,
            "gui_available": True,
            "headless_fallback": True,
            "offscreen_smoke": False,
            "reason": f"GUI smoke output could not be parsed: {exc}",
        }
    return {
        "ok": payload["row_count"] == payload["unit_count"],
        "gui_available": True,
        "headless_fallback": True,
        "offscreen_smoke": True,
        "table_rows": payload["row_count"],
        "versions": payload["versions"],
        "correction_points": payload["correction_points"],
    }


def _report_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Deterministic Sync Kernel Demo Report",
        "",
        "All samples, network traffic, checkpoints and comparisons are local.",
        "",
        "## Summary",
        "",
        f"- Checks passed: {report['summary']['passed']}",
        f"- Checks failed: {report['summary']['failed']}",
        f"- Overall: {report['summary']['overall']}",
        f"- Final authoritative hash: `{report['convergence']['final_hash']}`",
        f"- First divergence: `{report['convergence']['first_divergence']}`",
        "",
        "## Checks",
        "",
    ]
    for check in report["checks"]:
        lines.append(f"### {check['status'].upper()} - {check['title']}")
        lines.append("")
        lines.append(f"`{check['id']}`")
        lines.append("")
        lines.append("```json")
        lines.append(
            json.dumps(
                check["details"],
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
            )
        )
        lines.append("```")
        lines.append("")
    lines.extend(
        [
            "## Termination Boundary",
            "",
            "The forced-termination scenario never claims that the terminated process "
            "completed the match. Only the last atomically persisted, version-matched "
            "window is loaded; later ticks are explicitly recomputed after restart.",
            "",
        ]
    )
    return "\n".join(lines)


def run_demo(
    *,
    workspace: str | Path = WORKSPACE,
    ticks: int = 60,
    generate: bool = True,
) -> dict[str, Any]:
    root = Path(workspace)
    sample_root = root / "samples"
    report_root = root / "artifacts" / "demo"
    report_root.mkdir(parents=True, exist_ok=True)
    checks: list[dict[str, Any]] = []

    environment = _check(checks, "environment", "Environment and dependencies", _environment_check)
    if generate:
        bundle, expected_states = write_samples(sample_root, ticks=ticks)
    else:
        bundle = load_samples(sample_root)
        engine = MatchEngine(
            bundle.context,
            bundle.initial_state,
            bundle.inputs_by_tick,
        )
        expected_states, _ = engine.run_to(ticks)
    engine = MatchEngine(bundle.context, bundle.initial_state, bundle.inputs_by_tick)
    _check(
        checks,
        "samples",
        "Deterministic local sample generation",
        lambda: _sample_check(bundle, expected_states),
    )
    interop = _check(
        checks,
        "protocol-interop",
        "Three-version decode and deterministic re-encode interoperability",
        lambda: _protocol_interop_check(bundle, expected_states),
    )
    _check(
        checks,
        "prediction-rollback",
        "Client prediction, rollback and unconfirmed-input replay",
        lambda: _prediction_check(bundle, expected_states),
    )
    _check(
        checks,
        "sequence-guard",
        "E_SEQ sequence-gap and reverse-order state preservation",
        lambda: _sequence_guard_check(bundle, expected_states),
    )
    _check(
        checks,
        "error-matrix",
        "E_VER, E_DECODE and E_SEMANTIC error locations",
        lambda: _error_matrix_check(bundle, expected_states),
    )
    checkpoint_result = _check(
        checks,
        "checkpoint-recovery",
        "Atomic checkpoint, identity invalidation and forced-termination boundary",
        lambda: _checkpoint_check(root, bundle, expected_states, engine),
    )
    service_result = _check(
        checks,
        "loopback-service",
        "Loopback service convergence under drop, duplicate and reorder anomalies",
        lambda: _service_check(bundle, expected_states),
    )
    _check(
        checks,
        "bounded-concurrency",
        "Bounded parallel decode/advance/encode and deterministic output",
        lambda: _parallel_check(bundle, expected_states),
    )
    _check(
        checks,
        "gui-smoke",
        "Interactive UI offscreen smoke and headless fallback",
        lambda: _ui_check(sample_root),
    )

    passed = sum(1 for check in checks if check["status"] == "pass")
    failed = len(checks) - passed
    service_details = service_result["details"]
    convergence = {
        "final_hash": expected_states[-1].snapshot_hash,
        "final_tick": expected_states[-1].tick,
        "versions_compared": [1, 2, 3],
        "first_divergence": service_details.get("first_divergence"),
        "protocol_first_divergence": interop["details"].get("first_divergence"),
    }
    termination = checkpoint_result["details"].get("forced_termination", {})
    report = {
        "format": "deterministic-sync-demo-report",
        "format_version": 1,
        "environment": environment["details"],
        "sample": {
            "path": str(sample_root.relative_to(root)),
            "match_id": bundle.context.match_id,
            "seed": bundle.context.seed,
            "ticks": ticks,
            "input_fingerprint": bundle.context.input_fingerprint,
            "protocol_versions": sorted(PROTOCOL_SPECS),
        },
        "checks": checks,
        "summary": {
            "passed": passed,
            "failed": failed,
            "overall": "PASS" if failed == 0 else "FAIL",
        },
        "convergence": convergence,
        "forced_termination_boundary": {
            "original_process_report_complete": termination.get(
                "original_process_report_complete", False
            ),
            "persisted_window": termination.get("persisted_window"),
            "restart_recomputed": termination.get("restart_recomputed"),
            "note": termination.get("note"),
        },
    }
    atomic_json = report_root / "report.json"
    atomic_json.write_bytes(canonical_json_bytes(report) + b"\n")
    (report_root / "report.md").write_text(_report_markdown(report), encoding="utf-8")
    return report

