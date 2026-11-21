from __future__ import annotations

import json
import shutil
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any

from .conversion import export_score, parse_score
from .incremental import IncrementalPerformanceEngine, apply_edits
from .model import (
    ExpressionAnchor,
    Note,
    NoteEdit,
    Part,
    PedalSpan,
    Score,
    SolverParameters,
    TempoSegment,
    TimeSignature,
)
from .performance_validation import ValidationTolerance, validate_performance
from .publish import VersionedPublisher
from .recovery import RecoveryManager, SimulatedCrash
from .solver import PerformanceSolver, sequence_bytes
from .util import atomic_write_json, atomic_write_text, canonical_json_bytes, fraction_text, jsonl_bytes


GENERATOR_VERSION = "1.0"


class DeterministicRandom:
    def __init__(self, seed: int) -> None:
        self.state = (seed ^ 0x9E3779B97F4A7C15) & 0xFFFFFFFFFFFFFFFF

    def _next(self) -> int:
        value = self.state
        value ^= (value << 13) & 0xFFFFFFFFFFFFFFFF
        value ^= value >> 7
        value ^= (value << 17) & 0xFFFFFFFFFFFFFFFF
        self.state = value & 0xFFFFFFFFFFFFFFFF
        return self.state

    def between(self, low: int, high: int) -> int:
        return low + self._next() % (high - low + 1)


def _note(
    note_id: str,
    pitch: int,
    relative_start: Fraction,
    duration: Fraction,
    offset: Fraction,
    *,
    velocity: int,
    articulation: str,
) -> Note:
    return Note(
        id=note_id,
        pitch=pitch,
        start=offset + relative_start,
        duration=duration,
        velocity=velocity,
        articulation=articulation,
    )


def _classic_v1_score(seed: int) -> Score:
    rng = DeterministicRandom(seed + 1)
    offset = Fraction(3, 4)
    pitches = [60, 62, 64, 67, 69, 67, 64, 62, 60, 64, 67, 72, 71, 67, 64, 60]
    durations = [Fraction(1, 2), Fraction(1, 2), Fraction(1), Fraction(1, 2)] * 4
    notes = [
        _note(
            f"P1-N{index + 1:03d}",
            pitch,
            Fraction(index, 2),
            duration,
            offset,
            velocity=rng.between(72, 92),
            articulation="normal",
        )
        for index, (pitch, duration) in enumerate(zip(pitches, durations))
    ]
    return Score(
        title=f"Classic Import seed={seed}",
        composer="Local Generator",
        start_offset=offset,
        grid_resolution=Fraction(1, 8),
        time_signatures=(TimeSignature("meter-1", Fraction(0), 4, 4),),
        tempo_segments=(TempoSegment("tempo-1", Fraction(0), Fraction(90), None),),
        parts=(
            Part(
                "P1",
                "Piano",
                "piano",
                min(note.pitch for note in notes),
                max(note.pitch for note in notes),
                tuple(notes),
            ),
        ),
        expression=(),
        source_version=3,
        provenance={},
    )


def _expressive_v2_score(seed: int) -> Score:
    rng = DeterministicRandom(seed + 2)
    offset = Fraction(1, 2)
    signatures = (
        TimeSignature("meter-1", Fraction(0), 4, 4),
        TimeSignature("meter-2", Fraction(8), 3, 4),
        TimeSignature("meter-3", Fraction(14), 6, 8),
    )
    articulation_cycle = ["legato", "normal", "staccato", "legato"]
    part1_notes = []
    for index in range(12):
        part1_notes.append(
            _note(
                f"P1-N{index + 1:03d}",
                [60, 64, 67, 69, 72, 69, 67, 64, 62, 65, 69, 62][index],
                Fraction(index, 2) + (Fraction(1, 4) if index >= 6 else 0),
                Fraction(1, 2) if index % 3 == 2 else Fraction(1),
                offset,
                velocity=rng.between(68, 112),
                articulation=articulation_cycle[index % len(articulation_cycle)],
            )
        )
    part2_notes = []
    for index in range(8):
        part2_notes.append(
            _note(
                f"P2-N{index + 1:03d}",
                [48, 52, 55, 59, 60, 55, 52, 48][index],
                Fraction(index),
                Fraction(2) if index % 2 == 0 else Fraction(1),
                offset,
                velocity=rng.between(58, 94),
                articulation="legato" if index % 2 == 0 else "normal",
            )
        )
    return Score(
        title=f"Expressive Two-Hand seed={seed}",
        composer="Local Generator",
        start_offset=offset,
        grid_resolution=Fraction(1, 8),
        time_signatures=signatures,
        tempo_segments=(TempoSegment("tempo-1", Fraction(0), Fraction(108), None),),
        parts=(
            Part("P1", "Right Hand", "piano", 48, 84, tuple(part1_notes), (
                PedalSpan("P1-PEDAL-001", offset + 1, offset + 9, Fraction(4, 5)),
            )),
            Part("P2", "Left Hand", "piano", 36, 72, tuple(part2_notes), (
                PedalSpan("P2-PEDAL-001", offset + 2, offset + 7, Fraction(3, 4)),
                PedalSpan("P2-PEDAL-002", offset + 10, offset + 15, Fraction(4, 5)),
            )),
        ),
        expression=(),
        source_version=3,
        provenance={},
    )


def _virtual_v3_score(seed: int) -> Score:
    rng = DeterministicRandom(seed + 3)
    offset = Fraction(1, 4)
    signatures = (
        TimeSignature("meter-1", Fraction(0), 4, 4),
        TimeSignature("meter-2", Fraction(8), 3, 4),
        TimeSignature("meter-3", Fraction(14), 6, 8),
    )
    tempo = (
        TempoSegment("tempo-1", Fraction(0), Fraction(120), Fraction(8)),
        TempoSegment("tempo-2", Fraction(8), Fraction(90), Fraction(14)),
        TempoSegment("tempo-3", Fraction(14), Fraction(144), None),
    )
    arp_pitches = [72, 76, 79, 84, 79, 76, 74, 77, 81, 77, 74, 72]
    part1 = [
        _note(
            f"P1-N{index + 1:03d}",
            pitch,
            Fraction(index, 2),
            Fraction(1, 2) if index % 4 == 3 else Fraction(1),
            offset,
            velocity=rng.between(72, 112),
            articulation=["legato", "normal", "staccato", "normal"][index % 4],
        )
        for index, pitch in enumerate(arp_pitches)
    ]
    part2 = [
        _note(
            f"P2-N{index + 1:03d}",
            [55, 59, 62, 67, 64, 60, 57, 62][index],
            Fraction(index, 2) + Fraction(1, 4),
            Fraction(1),
            offset,
            velocity=rng.between(60, 100),
            articulation="legato" if index % 2 else "normal",
        )
        for index in range(8)
    ]
    part3 = [
        _note(
            f"P3-N{index + 1:03d}",
            [36, 43, 41, 48, 45, 43, 41, 36, 43, 48, 45, 38][index],
            Fraction(index * 2),
            Fraction(2),
            offset,
            velocity=rng.between(52, 88),
            articulation="legato",
        )
        for index in range(12)
    ]
    expression = (
        ExpressionAnchor("expr-1", offset, Fraction(2, 5), None),
        ExpressionAnchor("expr-2", offset + 6, Fraction(17, 20), None),
        ExpressionAnchor("expr-3", offset + 12, Fraction(11, 20), None),
        ExpressionAnchor("expr-4", offset + 18, Fraction(3, 4), None),
        ExpressionAnchor("expr-p3", offset + 3, Fraction(3, 10), "P3"),
    )
    return Score(
        title=f"Virtual Ensemble seed={seed}",
        composer="Local Generator",
        start_offset=offset,
        grid_resolution=Fraction(1, 8),
        time_signatures=signatures,
        tempo_segments=tempo,
        parts=(
            Part("P1", "Arpeggio", "virtual-synth", 48, 96, tuple(part1), (
                PedalSpan("P1-PEDAL-001", offset + 1, offset + 9, Fraction(4, 5)),
            )),
            Part("P2", "Counterline", "strings", 55, 79, tuple(part2), (
                PedalSpan("P2-PEDAL-001", offset + 7, offset + 15, Fraction(3, 5)),
            )),
            Part("P3", "Bass", "bass", 36, 60, tuple(part3), (
                PedalSpan("P3-PEDAL-001", offset + 2, offset + 6, Fraction(1, 2)),
            )),
        ),
        expression=expression,
        source_version=3,
        provenance={},
    )


def _relative_path(path: Path, root: Path) -> str:
    return path.relative_to(root).as_posix()


def _write_performance(path: Path, sequence: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(sequence_bytes(sequence))


def generate_samples(output_dir: str | Path, *, seed: int = 20261122) -> dict[str, Any]:
    root = Path(output_dir)
    scores_dir = root / "scores"
    baselines_dir = root / "baselines"
    recovery_dir = root / "recovery"
    manifests_dir = root / "manifests"
    for path in (scores_dir, baselines_dir, recovery_dir, manifests_dir):
        if path.exists():
            shutil.rmtree(path)
        path.mkdir(parents=True, exist_ok=True)
    trace: list[dict[str, Any]] = []
    parameters = SolverParameters()
    tolerance = ValidationTolerance()
    solver = PerformanceSolver(parameters)

    classic = _classic_v1_score(seed)
    expressive = _expressive_v2_score(seed)
    virtual = _virtual_v3_score(seed)
    v1_result = export_score(classic, 1, allow_loss=True)
    v2_result = export_score(expressive, 2, allow_loss=True)
    v3_result = export_score(virtual, 3)
    native_payloads = {
        "classic_v1.json": (1, v1_result.payload),
        "expressive_v2.json": (2, v2_result.payload),
        "virtual_v3.json": (3, v3_result.payload),
    }
    for filename, (version, payload) in native_payloads.items():
        path = scores_dir / filename
        atomic_write_json(path, payload)
        trace.append(
            {
                "event": "sample_generated",
                "file": _relative_path(path, root),
                "version": version,
                "seed": seed,
            }
        )

    range_payload = json.loads(canonical_json_bytes(v3_result.payload))
    range_part = next(part for part in range_payload["parts"] if part["id"] == "P2")
    range_part["notes"][2]["pitch"] = range_part["pitch_range"]["max"] + 8
    range_path = scores_dir / "range_violation_v3.json"
    atomic_write_json(range_path, range_payload)
    trace.append(
        {
            "event": "sample_generated",
            "file": _relative_path(range_path, root),
            "version": 3,
            "seed": seed,
            "purpose": "pitch_range_validation",
        }
    )

    exported_files: dict[str, tuple[int, dict[str, Any]]] = {}
    v3_to_v2 = export_score(virtual, 2, allow_loss=True)
    v3_to_v1 = export_score(virtual, 1, allow_loss=True)
    v1_upgraded = export_score(parse_score(scores_dir / "classic_v1.json"), 3, allow_loss=True)
    v2_upgraded = export_score(parse_score(scores_dir / "expressive_v2.json"), 3, allow_loss=True)
    combos = {
        "virtual_v3_to_v2.json": (2, v3_to_v2),
        "virtual_v3_to_v1.json": (1, v3_to_v1),
        "classic_v1_upgraded_v3.json": (3, v1_upgraded),
        "expressive_v2_upgraded_v3.json": (3, v2_upgraded),
    }
    for filename, (version, result) in combos.items():
        path = scores_dir / filename
        atomic_write_json(path, result.payload)
        exported_files[filename] = (version, result.payload)
        trace.append(
            {
                "event": "version_conversion",
                "file": _relative_path(path, root),
                "target_version": version,
                "loss_count": result.loss_count,
                "loss_fields": sorted({loss["field"] for loss in result.losses}),
            }
        )

    baseline_entries: list[dict[str, Any]] = []
    all_score_files = sorted(
        {
            *[scores_dir / filename for filename in native_payloads],
            range_path,
            *[scores_dir / filename for filename in combos],
        },
        key=lambda path: path.name,
    )
    for path in all_score_files:
        score = parse_score(path)
        sequence = solver.solve(score, parameters=parameters)
        performance_path = baselines_dir / f"{path.stem}.performance.json"
        _write_performance(performance_path, sequence)
        validation = validate_performance(
            score,
            sequence,
            tolerance=tolerance,
            parameters=parameters,
        )
        entry = {
            "file": _relative_path(path, root),
            "performance_file": _relative_path(performance_path, root),
            "source_version": score.source_version,
            "part_count": len(score.parts),
            "note_count": score.note_count(),
            "score_hash": score.score_hash(),
            "baseline_sequence_hash": sequence["content_hash"],
            "validation_within_tolerance": validation["summary"]["within_tolerance"],
            "violation_count": validation["summary"]["violation_count"],
            "maximum_deviation_note": validation["maximum_deviation_note"],
        }
        baseline_entries.append(entry)
        trace.append(
            {
                "event": "solve",
                "file": _relative_path(path, root),
                "version": score.source_version,
                "part_count": len(score.parts),
                "note_count": score.note_count(),
                "affected_note_count": score.note_count(),
                "reused_note_count": 0,
                "violation_count": validation["summary"]["violation_count"],
                "maximum_deviation_note": validation["maximum_deviation_note"],
                "deviation_statistics": validation["summary"]["statistics"],
                "content_hash": sequence["content_hash"],
            }
        )

    trace.extend(
        _incremental_trace(
            root,
            scores_dir / "virtual_v3.json",
            baselines_dir,
            parameters,
            tolerance,
        )
    )
    trace.extend(_recovery_trace(root, recovery_dir, scores_dir / "virtual_v3.json", parameters))

    manifest = {
        "schema": "deterministic-sample-manifest-v1",
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "parameters": parameters.to_dict(),
        "tolerance": tolerance.to_dict(),
        "scores": baseline_entries,
    }
    atomic_write_json(manifests_dir / "baseline-manifest.json", manifest)
    atomic_write_json(
        manifests_dir / "conversion-report.json",
        {
            "schema": "conversion-report-v1",
            "native_losses": {
                "classic_v1.json": v1_result.losses,
                "expressive_v2.json": v2_result.losses,
                "virtual_v3.json": v3_result.losses,
            },
            "exports": {
                filename: {
                    "target_version": version,
                    "loss_count": len(result.losses),
                    "losses": result.losses,
                }
                for filename, (version, result) in combos.items()
            },
        },
    )
    atomic_write_text(root / "trace.jsonl", jsonl_bytes(trace).decode("utf-8"))
    return {
        "root": str(root),
        "manifest": manifest,
        "trace_records": len(trace),
        "trace_path": str(root / "trace.jsonl"),
    }


def _incremental_trace(
    root: Path,
    score_path: Path,
    baselines_dir: Path,
    parameters: SolverParameters,
    tolerance: ValidationTolerance,
) -> list[dict[str, Any]]:
    score = parse_score(score_path)
    solver = PerformanceSolver(parameters)
    publisher = VersionedPublisher(root / "manifests" / "incremental")
    engine = IncrementalPerformanceEngine(solver=solver, publisher=publisher)
    initial = engine.solve_initial(score, parameters=parameters, version=1)
    trace: list[dict[str, Any]] = [
        {
            "event": "incremental_initial",
            "file": _relative_path(score_path, root),
            "version": 1,
            "part_count": len(score.parts),
            "affected_note_count": initial.affected_count,
            "reused_note_count": initial.reused_count,
            "violation_count": 0,
            "content_hash": initial.snapshot.content_hash,
        }
    ]
    operations: list[tuple[str, NoteEdit]] = []
    anchor = score.expression[1]
    operations.append(
        (
            "expression_anchor_value",
            NoteEdit(
                "expression_anchor",
                index=1,
                value=Fraction(1, 10),
                old_value=anchor.value,
            ),
        )
    )
    tempo = score.tempo_segments[1]
    operations.append(
        (
            "tempo_bpm",
            NoteEdit("tempo_segment", index=1, value=Fraction(80), old_value=tempo),
        )
    )
    part1 = score.parts[0]
    pedal = part1.pedals[0]
    operations.append(
        (
            "pedal_value",
            NoteEdit(
                "pedal_span",
                part_id=part1.id,
                index=0,
                value=Fraction(3, 5),
                old_value=pedal,
            ),
        )
    )
    first_note = part1.notes[0]
    operations.append(
        (
            "note_velocity",
            NoteEdit(
                "note_velocity",
                part_id=part1.id,
                note_id=first_note.id,
                value=105,
                old_value=first_note.velocity,
            ),
        )
    )
    current = score
    for index, (name, edit) in enumerate(operations, start=2):
        current = apply_edits(current, [edit])
        result = engine.apply(current, [edit], parameters=parameters)
        validation = validate_performance(
            current,
            result.sequence,
            tolerance=tolerance,
            parameters=parameters,
        )
        _write_performance(
            baselines_dir / f"incremental-{index - 1}.performance.json",
            result.sequence,
        )
        trace.append(
            {
                "event": "incremental_solve",
                "operation": name,
                "version": result.snapshot.version,
                "affected_note_count": result.affected_count,
                "reused_note_count": result.reused_count,
                "affected_note_ids": list(result.affected_note_ids),
                "reused_note_ids": list(result.reused_note_ids),
                "reasons": dict(sorted(result.reasons.items())),
                "edits": result.edits,
                "violation_count": validation["summary"]["violation_count"],
                "part_count": len(current.parts),
                "maximum_deviation_note": validation["maximum_deviation_note"],
                "deviation_statistics": validation["summary"]["statistics"],
                "content_hash": result.snapshot.content_hash,
            }
        )
    fresh_sequence = PerformanceSolver(parameters).solve(current, parameters=parameters)
    equivalent = sequence_bytes(fresh_sequence) == sequence_bytes(result.sequence)
    trace.append(
        {
            "event": "incremental_equivalence",
            "equivalent": equivalent,
            "incremental_hash": result.snapshot.content_hash,
            "fresh_hash": fresh_sequence["content_hash"],
        }
    )
    if not equivalent:
        raise RuntimeError("增量重解结果与全量重算不一致")
    return trace


def _recovery_trace(
    root: Path,
    recovery_dir: Path,
    score_path: Path,
    parameters: SolverParameters,
) -> list[dict[str, Any]]:
    score = parse_score(score_path)
    trace: list[dict[str, Any]] = []
    manager = RecoveryManager(
        recovery_dir,
        score,
        parameter_version=7,
        sequence_version=1,
        parameters=parameters,
    )
    try:
        manager.resume(crash_after_new_parts=2)
    except SimulatedCrash as exc:
        trace.append(
            {
                "event": "simulated_forced_termination",
                "message": str(exc),
                "committed_checkpoints": 2,
            }
        )
    result = manager.resume()
    fresh = PerformanceSolver(parameters).solve(score, parameters=parameters)
    equivalent = sequence_bytes(fresh) == sequence_bytes(result.sequence)
    trace.append(
        {
            "event": "recovery_completed",
            "resumed_part_ids": list(result.resumed_part_ids),
            "newly_solved_part_ids": list(result.newly_solved_part_ids),
            "recovered_note_count": len(result.recovered_note_ids),
            "newly_solved_note_count": len(result.newly_solved_note_ids),
            "chain_tip": result.chain_tip,
            "equivalent_to_fresh_solve": equivalent,
            "sequence_version": 1,
            "part_count": len(score.parts),
            "content_hash": result.sequence["content_hash"],
        }
    )
    if not equivalent:
        raise RuntimeError("恢复结果与全量重算不一致")
    return trace

