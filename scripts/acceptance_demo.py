"""End-to-end acceptance driver.

Runs every acceptance scenario against the deterministic samples and prints a
machine-readable report.  Usage::

    python -m scripts.acceptance_demo --samples samples --report artifacts/acceptance_report.json
"""

from __future__ import annotations

import argparse
import json
import tempfile
from pathlib import Path

from scoreworkbench.errors import WorkbenchError
from scoreworkbench.persistence import SnapshotStore
from scoreworkbench.playback import FakeClock, PlaybackEngine
from scoreworkbench.session import EditSession
from scoreworkbench.versioning import (
    export_score,
    load_many,
    load_score,
    roundtrip_check,
)


def _record(results, name, func):
    try:
        payload = func()
        results.append({"scenario": name, "status": "PASS", "detail": payload})
    except WorkbenchError as error:
        results.append(
            {"scenario": name, "status": "FAIL",
             "detail": error.to_dict()}
        )


def run(sample_root: Path) -> dict:
    results: list[dict] = []
    sample_paths = [
        sample_root / "score_v1.json",
        sample_root / "score_v2.json",
        sample_root / "score_v3.json",
    ]

    def scenario_load():
        projects = load_many([str(path) for path in sample_paths])
        return {
            "loaded": [
                {
                    "version": project.source_version,
                    "notes": len(project.score.notes),
                    "events": len(project.score.events),
                    "traces": len(project.score.traces),
                    "default_fills": sum(
                        1 for t in project.score.traces
                        if t.action == "default_fill"
                    ),
                }
                for project in projects
            ]
        }

    def scenario_roundtrip():
        return {"checks": [roundtrip_check(path) for path in sample_paths]}

    def scenario_lossy():
        project = load_score(sample_root / "score_v3.json")
        warned = {}
        for target in ("1.0", "2.0"):
            try:
                export_score(project.score, target, project.provenance)
            except WorkbenchError as error:
                warned[target] = error.details
        confirmed = export_score(
            project.score, "1.0", project.provenance, confirm_lossy=True
        )
        return {
            "warning_counts": {
                target: len(detail["warnings"])
                for target, detail in warned.items()
            },
            "confirmed_export_events": len(confirmed["expression_events"]),
        }

    def scenario_edit_consistency():
        project = load_score(sample_root / "score_v3.json")
        session = EditSession(project.score)
        note = session.working.notes[0]
        before = [
            (event.measure, event.anchor_beat)
            for event in session.working.events_for_note(note.note_id)
        ]
        session.edit_note(
            note.note_id, measure=note.measure + 1,
            start_beat=note.start_beat + 0.25,
        )
        after = [
            (event.measure, event.anchor_beat)
            for event in session.working.events_for_note(note.note_id)
        ]
        record = session.commit()
        session.committed.validate_references()
        return {
            "migrated": before != after,
            "affected_objects": record.affected_objects,
            "dangling_check": "ok",
        }

    def scenario_snapshot():
        project = load_score(sample_root / "score_v2.json")
        with tempfile.TemporaryDirectory() as directory:
            store = SnapshotStore(directory)
            store.save(project.score)
            bundle = store.load()
            same = bundle.score.to_dict() == project.score.to_dict()
            store.invalidate()
            invalidated = not store.exists()
        return {"restored_identical": same, "invalidated": invalidated}

    def scenario_playback():
        project = load_score(sample_root / "score_v3.json")
        clock = FakeClock()
        engine = PlaybackEngine(project.score, clock)
        engine.validate_event_ranges()
        engine.play(start_beat=0.0)
        clock.advance(1.0)
        frame = engine.frame()
        paused = engine.pause()
        return {
            "beat_after_1s": round(frame.current_beat, 6),
            "seconds_after_1s": round(frame.elapsed_seconds, 6),
            "paused_beat": round(paused, 6),
            "velocity_samples": int(engine.velocity_curve(64).shape[0]),
            "legato_spans": len(engine.legato_spans()),
        }

    def scenario_error_codes():
        observed = {}
        broken = sample_root / "broken"
        probes = {
            "E_DECODE": broken / "corrupted.json",
            "E_SCHEMA_VERSION": broken / "unknown_version.json",
            "E_SCHEMA_FIELD": broken / "missing_required.json",
            "E_DANGLE": broken / "dangling_event.json",
            "E_SEMANTIC": broken / "semantic_conflict.json",
        }
        for label, path in probes.items():
            try:
                load_score(path)
            except WorkbenchError as error:
                observed[label] = {
                    "code": error.code,
                    "details_keys": sorted(error.details),
                }
        return observed

    for name, func in [
        ("multi_version_load", scenario_load),
        ("roundtrip_equivalence", scenario_roundtrip),
        ("lossy_downgrade_warnings", scenario_lossy),
        ("edit_consistency", scenario_edit_consistency),
        ("snapshot_restore", scenario_snapshot),
        ("playback_timeline", scenario_playback),
        ("error_codes", scenario_error_codes),
    ]:
        _record(results, name, func)

    summary = {
        "total": len(results),
        "passed": sum(1 for item in results if item["status"] == "PASS"),
        "failed": sum(1 for item in results if item["status"] == "FAIL"),
    }
    return {"summary": summary, "results": results}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", default="samples")
    parser.add_argument("--report", default="artifacts/acceptance_report.json")
    args = parser.parse_args()
    report = run(Path(args.samples))
    report_path = Path(args.report)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    with open(report_path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, indent=2, sort_keys=True)
    print(json.dumps(report["summary"], indent=2))
    for item in report["results"]:
        print(f"- {item['scenario']}: {item['status']}")


if __name__ == "__main__":
    main()
