"""Scripted deterministic online/offline demonstration and trace logging."""

from __future__ import annotations

import hashlib
import json
import math
import os
from collections.abc import Iterable
from pathlib import Path
from typing import Any

from .analyzer import AnalysisResult, analyze_audio, compare_online_offline
from .params import (
    AnalysisParameters,
    canonical_json,
    downmix_51_to_stereo,
)
from .report import (
    load_report,
    save_report,
    save_report_document,
)
from .session import MeterSession, SessionManager
from .synth import generate_sample_file
from .wavio import AudioBuffer, read_wav


def default_operations() -> dict[str, Any]:
    operations: list[dict[str, Any]] = [
        {"op": "play"},
        {"op": "advance", "advance_ms": 600},
        {"op": "refresh", "count": 3},
        {"op": "change_params", "target_lufs": -20.0},
        {"op": "advance", "advance_ms": 900},
        {"op": "refresh", "count": 2},
        {"op": "change_params", "true_peak_limit_dbtp": -12.0},
        {"op": "advance", "advance_ms": 900},
        {"op": "refresh", "count": 2},
        {"op": "change_params", "relative_gate_lu": -8.0},
        {"op": "advance", "advance_ms": 900},
        {"op": "refresh", "count": 2},
        {
            "op": "change_params",
            "weights": [0.85, 0.9, 0.8, 0.0, 1.25, 1.2],
        },
        {"op": "advance", "advance_ms": 900},
        {"op": "refresh", "count": 2},
        {"op": "pause"},
        {"op": "seek", "sample": 2 * 48_000 + 960},
        {"op": "refresh"},
        {"op": "play"},
        {"op": "advance", "advance_ms": 1200},
        {"op": "refresh", "count": 2},
        {"op": "restart"},
        {"op": "play"},
        {"op": "advance", "advance_ms": 800},
        {"op": "refresh"},
    ]
    return {"initial_params": {}, "operations": operations}


def load_operations(path: str | os.PathLike[str] | None) -> dict[str, Any]:
    if path is None:
        return default_operations()
    value = json.loads(Path(path).read_text(encoding="utf-8"))
    if isinstance(value, list):
        return {"initial_params": {}, "operations": value}
    if not isinstance(value, dict) or not isinstance(value.get("operations"), list):
        raise TypeError("operation script must contain an operations list")
    return value


def run_demo(
    *,
    seed: int,
    operations_document: dict[str, Any],
    output_directory: str | os.PathLike[str],
    channels: int = 6,
    duration_seconds: float = 12.0,
    bit_depth: int = 24,
) -> dict[str, Any]:
    output = Path(output_directory)
    output.mkdir(parents=True, exist_ok=True)
    sample_path = output / "sample.wav"
    manifest = generate_sample_file(
        sample_path,
        seed=seed,
        channels=channels,
        duration_seconds=duration_seconds,
        bit_depth=bit_depth,
    )
    audio = read_wav(sample_path)
    initial = dict(operations_document.get("initial_params") or {})
    params = _parameters_from_updates(audio, initial, None)

    manager = SessionManager()
    session = manager.open_session(audio, params)
    trace_events: list[dict[str, Any]] = [
        {
            "event": "session_start",
            "seed": seed,
            "source_sha256": audio.source_sha256,
            "initial_parameter_version": session.parameter_version,
            "block_samples": params.block_samples,
            "hop_samples": params.hop_samples,
        }
    ]
    refresh_index = 0
    parameter_results: list[dict[str, Any]] = []
    checkpoint_path = output / "checkpoint.json"
    checkpoint_saved = False
    effective_operations = list(operations_document["operations"])

    for operation_index, operation in enumerate(effective_operations):
        op = str(operation.get("op", ""))
        trace_events.append(
            {"event": "operation", "operation_index": operation_index, "op": op}
        )
        if op == "play":
            session.play()
        elif op == "pause":
            session.pause()
        elif op == "advance":
            session.advance(_advance_samples(operation))
        elif op == "seek":
            session.seek(_seek_sample(operation, audio, params))
        elif op == "restart":
            session.restart()
        elif op == "change_params":
            updates = {
                key: value
                for key, value in operation.items()
                if key not in {"op", "downmix_preset"}
            }
            new_params = _parameters_from_updates(audio, updates, session.params)
            before = session.snapshot()
            snapshot = session.change_parameters(
                target_lufs=new_params.target_lufs,
                true_peak_limit_dbtp=new_params.true_peak_limit_dbtp,
                absolute_gate_lufs=new_params.absolute_gate_lufs,
                relative_gate_lu=new_params.relative_gate_lu,
                weights=new_params.weights,
                downmix=new_params.downmix,
                output_layout=new_params.output_layout,
            )
            if snapshot.version != before.version:
                parameter_results.append(
                    {
                        "parameter_version": snapshot.parameter_version,
                        "affected_blocks": list(snapshot.affected_blocks),
                        "affected_count": len(snapshot.affected_blocks),
                        "cache_stats": snapshot.cache_stats,
                    }
                )
                trace_events.append(
                    {
                        "event": "parameter_publish",
                        "parameter_version": snapshot.parameter_version,
                        "snapshot_version": snapshot.version,
                        "affected_blocks": list(snapshot.affected_blocks),
                        "recomputed_measurement_blocks": snapshot.cache_stats[
                            "recomputed_measurement_blocks"
                        ],
                    }
                )
        elif op == "refresh":
            count = max(1, int(operation.get("count", 1)))
            for _ in range(count):
                refresh_index = _append_refresh(
                    trace_events,
                    session,
                    refresh_index,
                    operation_index,
                )
        elif op == "save_checkpoint":
            save_report(checkpoint_path, audio, session.snapshot())
            checkpoint_saved = True
            trace_events.append(
                {
                    "event": "checkpoint",
                    "committed_blocks": session.snapshot().committed_count,
                    "parameter_version": session.parameter_version,
                }
            )
        elif op == "advance_to_end":
            session.advance(audio.frame_count - session.position_sample)
        elif op == "close":
            break
        else:
            raise ValueError(f"unknown operation: {op}")

    if not checkpoint_saved:
        save_report(checkpoint_path, audio, session.snapshot())
        trace_events.append(
            {
                "event": "checkpoint",
                "committed_blocks": session.snapshot().committed_count,
                "parameter_version": session.parameter_version,
            }
        )

    if session.position_sample < audio.frame_count:
        session.advance(audio.frame_count - session.position_sample)
    refresh_index = _append_refresh(
        trace_events,
        session,
        refresh_index,
        len(effective_operations),
        final=True,
    )
    final_snapshot = session.snapshot()
    final_params = session.params
    offline = analyze_audio(audio, final_params)
    consistency = compare_online_offline(
        final_snapshot.blocks,
        offline.blocks,
        tolerance=1e-6,
    )

    report_path = output / "report.json"
    save_report(report_path, audio, final_snapshot)
    trace_path = output / "trace.jsonl"
    _write_trace(trace_path, trace_events)
    offline_path = output / "offline.json"
    save_report_document(
        offline_path,
        _offline_document(audio, offline, manifest),
    )

    recovered = load_report(checkpoint_path, audio=audio)
    reference_delta = None
    if final_params.digest() == AnalysisParameters.for_channels(channels).digest():
        reference_value = manifest.get("reference", {}).get("integrated_lufs")
        if reference_value is not None and offline.integrated_lufs is not None:
            reference_delta = offline.integrated_lufs - float(reference_value)

    validation = {
        "seed": seed,
        "source_sha256": audio.source_sha256,
        "online_offline": consistency,
        "offline_integrated_lufs": offline.integrated_lufs,
        "online_integrated_lufs": final_snapshot.integrated_lufs,
        "reference_integrated_lufs": manifest.get("reference", {}).get(
            "integrated_lufs"
        ),
        "reference_integrated_delta_lu": reference_delta,
        "final_parameter_version": final_snapshot.parameter_version,
        "final_snapshot_version": final_snapshot.version,
        "final_committed_blocks": final_snapshot.committed_count,
        "parameter_changes": parameter_results,
        "checkpoint": {
            "filename": checkpoint_path.name,
            "committed_blocks": len(recovered.blocks),
            "chain_head": recovered.chain_head,
        },
        "artifacts": {
            "report_sha256": hashlib.sha256(report_path.read_bytes()).hexdigest(),
            "trace_sha256": hashlib.sha256(trace_path.read_bytes()).hexdigest(),
            "offline_sha256": hashlib.sha256(offline_path.read_bytes()).hexdigest(),
        },
    }
    save_report_document(output / "validation.json", validation)
    session.close()
    return {
        "manifest": manifest,
        "report": str(report_path),
        "trace": str(trace_path),
        "offline": str(offline_path),
        "checkpoint": str(checkpoint_path),
        "validation": validation,
    }


def _append_refresh(
    events: list[dict[str, Any]],
    session: MeterSession,
    refresh_index: int,
    operation_index: int,
    *,
    final: bool = False,
) -> int:
    view = session.view()
    snapshot = view.snapshot
    latest = snapshot.blocks[-1] if snapshot.blocks else None
    event: dict[str, Any] = {
        "event": "refresh",
        "refresh_index": refresh_index,
        "operation_index": operation_index,
        "final": final,
        "playback_position_sample": view.playback_position_sample,
        "current_block_index": view.current_block_index,
        "committed_blocks": snapshot.committed_count,
        "snapshot_version": snapshot.version,
        "parameter_version": snapshot.parameter_version,
        "playing": view.playing,
    }
    if latest is None:
        event.update(
            {
                "observation_block_index": None,
                "momentary_lufs": None,
                "short_lufs": None,
                "integrated_lufs": None,
                "true_peak_dbtp": None,
                "content_hash": None,
                "measurement_hash": None,
            }
        )
    else:
        event.update(
            {
                "observation_block_index": latest.index,
                "momentary_lufs": latest.momentary_lufs,
                "short_lufs": latest.short_lufs,
                "integrated_lufs": snapshot.integrated_lufs,
                "true_peak_dbtp": latest.true_peak_dbtp,
                "content_hash": latest.content_hash,
                "measurement_hash": latest.measurement_hash,
            }
        )
    events.append(event)
    return refresh_index + 1


def _advance_samples(operation: dict[str, Any]) -> int:
    if "advance_samples" in operation:
        return int(operation["advance_samples"])
    if "advance_ms" in operation:
        value = float(operation["advance_ms"]) * 48.0
        rounded = round(value)
        if not math.isclose(value, rounded, abs_tol=1e-9):
            raise ValueError("advance_ms must resolve to a whole sample")
        return rounded
    return int(operation.get("samples", 0))


def _seek_sample(
    operation: dict[str, Any],
    audio: AudioBuffer,
    params: AnalysisParameters,
) -> int:
    if "sample" in operation:
        return int(operation["sample"])
    if "seconds" in operation:
        return round(float(operation["seconds"]) * audio.sample_rate)
    if "block" in operation:
        block = max(0, int(operation["block"]))
        return min(
            audio.frame_count,
            block * params.hop_samples + params.block_samples,
        )
    return 0


def _parameters_from_updates(
    audio: AudioBuffer,
    updates: dict[str, Any],
    current: AnalysisParameters | None,
) -> AnalysisParameters:
    preset = updates.pop("downmix_preset", None)
    if preset is not None:
        if preset == "identity":
            base = AnalysisParameters.for_channels(audio.channels)
        elif preset == "stereo":
            if audio.channels != 6:
                raise ValueError("stereo downmix preset requires 5.1 input")
            base = AnalysisParameters.for_channels(
                audio.channels,
                downmix=downmix_51_to_stereo(),
                weights=(1.0, 1.0),
                output_layout=("L", "R"),
            )
        else:
            raise ValueError(f"unknown downmix preset: {preset}")
        values = base.as_dict()
        for key in (
            "target_lufs",
            "true_peak_limit_dbtp",
            "absolute_gate_lufs",
            "relative_gate_lu",
            "weights",
            "downmix",
            "output_layout",
        ):
            updates.setdefault(key, values[key])
    if current is None:
        return AnalysisParameters.for_channels(
            audio.channels,
            **{
                key: tuple(value)
                if key in {"weights", "output_layout"}
                else tuple(tuple(row) for row in value)
                if key == "downmix"
                else value
                for key, value in updates.items()
            },
        )
    return current.with_updates(channel_count=audio.channels, **updates)


def _offline_document(
    audio: AudioBuffer,
    result: AnalysisResult,
    manifest: dict[str, Any],
) -> dict[str, Any]:
    max_peak_block = max(
        (block for block in result.blocks if block.true_peak_dbtp is not None),
        key=lambda block: block.true_peak_dbtp or -999.0,
        default=None,
    )
    return {
        "schema": "broadcast-loudness-offline-baseline",
        "report_version": 1,
        "source": {
            "filename": Path(audio.filename).name,
            "sha256": audio.source_sha256,
            "sample_rate": audio.sample_rate,
            "bit_depth": audio.bit_depth,
            "channels": audio.channels,
            "layout": list(audio.layout),
            "frames": audio.frame_count,
        },
        "parameters": result.params.as_dict(),
        "summary": {
            "integrated_lufs": result.integrated_lufs,
            "true_peak_dbtp": (
                max_peak_block.true_peak_dbtp if max_peak_block else None
            ),
            "true_peak_position_sample": (
                max_peak_block.true_peak_position_sample if max_peak_block else None
            ),
            "true_peak_channel": (
                max_peak_block.true_peak_channel if max_peak_block else None
            ),
            "block_count": len(result.blocks),
        },
        "reference": manifest.get("reference"),
        "blocks": [block.as_dict() for block in result.blocks],
    }


def _write_trace(path: Path, events: Iterable[dict[str, Any]]) -> None:
    lines = [canonical_json(event).encode("utf-8") + b"\n" for event in events]
    payload = b"".join(lines)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
