#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from streamforge.config import load_project
from streamforge.generator import atomic_write
from streamforge.models import canonical_json
from streamforge.pipeline import Pipeline, load_artifacts_from_output
from streamforge.playback import PlaybackConfig, load_bandwidth, simulate_playback
from streamforge.samples import generate_samples


def digest_tree(root: Path) -> dict[str, str]:
    return {
        str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def main() -> int:
    samples = ROOT / "samples"
    demo_output = ROOT / "artifacts" / "demo"
    failure_output = ROOT / "artifacts" / "failure"
    determinism_output = ROOT / "artifacts" / "determinism"
    paths = generate_samples(samples, seed=20261121, duration_ms=3200, overwrite=True)
    for output in (demo_output, failure_output, determinism_output):
        if output.exists():
            shutil.rmtree(output)

    pipeline = Pipeline(
        config_path=paths["track_config"],
        budget_path=paths["budgets"],
        output_root=demo_output,
    )
    run = pipeline.run()
    artifacts = load_artifacts_from_output(demo_output, pipeline.project)
    scheduler_trace_bytes = run.trace_path.read_bytes()

    timeline_checks = []
    for tier_id, slices in artifacts.items():
        checks = len(slices) == len(run.timeline.boundaries)
        for index, artifact in enumerate(slices):
            boundary = run.timeline.boundaries[index]
            checks = checks and (
                artifact.index == index
                and artifact.start_ns == boundary.start_ns
                and artifact.end_ns == boundary.end_ns
                and artifact.audio_start_ns == boundary.start_ns
                and artifact.video_start_ns == boundary.start_ns
            )
            if index:
                previous = slices[index - 1]
                checks = checks and (
                    previous.end_ns == artifact.start_ns
                    and previous.audio_sample_end == artifact.audio_sample_start
                )
        timeline_checks.append({"tier": tier_id, "aligned": checks})

    playback = PlaybackConfig.from_dict(
        json.loads(paths["playback"].read_text(encoding="utf-8"))
    )
    playback_result = simulate_playback(
        tiers=pipeline.project.tiers,
        slices=artifacts,
        bandwidth=load_bandwidth(paths["bandwidth"]),
        playback=playback,
        budget=pipeline.project.budget,
    )
    switches_aligned = all(
        event["index"] < len(run.timeline.boundaries)
        and artifacts[event["to_tier"]][event["index"]].start_ns
        == run.timeline.boundaries[event["index"]].start_ns
        for event in playback_result.events
        if event["event"] == "switch"
    )
    playback_lines = [canonical_json(event) for event in playback_result.events]
    atomic_write(
        demo_output / "playback_trace.jsonl",
        b"\n".join(playback_lines) + (b"\n" if playback_lines else b""),
    )
    scheduler_documents = [
        json.loads(line)
        for line in scheduler_trace_bytes.decode("utf-8").splitlines()
        if line
    ]
    demo_lines = [
        canonical_json(document) for document in scheduler_documents
    ] + [canonical_json({"source": "playback", **event}) for event in playback_result.events]
    atomic_write(
        demo_output / "demo_trace.jsonl",
        b"\n".join(demo_lines) + (b"\n" if demo_lines else b""),
    )

    snapshot_bytes_before = run.snapshot_path.read_bytes()
    restore = Pipeline(
        config_path=paths["track_config"],
        budget_path=paths["budgets"],
        output_root=demo_output,
    ).run(restore_snapshot=run.snapshot_path)
    snapshot_bytes_after = run.snapshot_path.read_bytes()
    demo_summary = {
        "errors": run.errors,
        "memory_peak_bytes": run.scheduler.peak_bytes,
        "playback": {
            "errors": playback_result.errors,
            "final_tier": playback_result.final_tier,
            "switch_count": playback_result.switch_count,
        },
        "snapshot": "artifacts/demo/snapshot.json",
        "status": run.status,
        "tiers": {
            tier_id: {
                "completed": len(items),
                "hashes": [item.content_hash for item in items],
            }
            for tier_id, items in artifacts.items()
        },
        "trace": "artifacts/demo/demo_trace.jsonl",
    }
    atomic_write(
        demo_output / "demo_summary.json",
        canonical_json(demo_summary) + b"\n",
    )

    failure = Pipeline(
        config_path=paths["track_config"],
        budget_path=paths["budgets"],
        output_root=failure_output,
    ).run(fault_at=("low", 1))
    failure_low_files = sorted(path.name for path in (failure_output / "low").glob("*.fslice"))
    failure_other_counts = {
        tier: len(list((failure_output / tier).glob("*.fslice")))
        for tier in ("mid", "high")
    }
    temporary_files = [
        str(path.relative_to(ROOT))
        for path in ROOT.rglob(".*.tmp")
        if path.is_file()
    ]

    deterministic = Pipeline(
        config_path=paths["track_config"],
        budget_path=paths["budgets"],
        output_root=determinism_output,
    ).run()
    tree_a = digest_tree(demo_output)
    tree_b = digest_tree(determinism_output)
    deterministic_slices = {
        name: digest
        for name, digest in tree_a.items()
        if name.endswith(".fslice")
    } == {
        name: digest
        for name, digest in tree_b.items()
        if name.endswith(".fslice")
    }

    report = {
        "budget": {
            "peak_bytes": run.scheduler.peak_bytes,
            "peak_within_limit": run.scheduler.peak_bytes
            <= pipeline.project.budget.total_memory_bytes,
            "total_limit_bytes": pipeline.project.budget.total_memory_bytes,
            "memory_samples_within_limit": all(
                sample["allocated_bytes"]
                <= pipeline.project.budget.total_memory_bytes
                for sample in run.scheduler.memory_samples
            ),
        },
        "determinism": {
            "scheduler_trace_identical": (
                scheduler_trace_bytes
                == (determinism_output / "scheduler_trace.jsonl").read_bytes()
            ),
            "slice_files_identical": deterministic_slices,
            "snapshot_identical": (
                (demo_output / "snapshot.json").read_bytes()
                == (determinism_output / "snapshot.json").read_bytes()
            ),
        },
        "failure_isolation": {
            "errors": failure.errors,
            "low_files": failure_low_files,
            "other_tier_completed_slices": failure_other_counts,
            "status": failure.status,
            "temporary_files_remaining": temporary_files,
        },
        "playback": {
            "errors": playback_result.errors,
            "final_tier": playback_result.final_tier,
            "switch_count": playback_result.switch_count,
            "switches_aligned": switches_aligned,
        },
        "snapshot_restore": {
            "recomputed_tiers": restore.recomputed_tiers,
            "reused_tiers": restore.reused_tiers,
            "snapshot_bytes_identical": snapshot_bytes_before == snapshot_bytes_after,
            "status": restore.status,
        },
        "slicing": {
            "errors": run.errors,
            "status": run.status,
            "timeline_checks": timeline_checks,
            "trace": "artifacts/demo/demo_trace.jsonl",
        },
    }
    report_path = ROOT / "artifacts" / "acceptance_report.json"
    atomic_write(report_path, canonical_json(report) + b"\n")
    print(json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2))
    return (
        0
        if run.status == "complete"
        and not run.errors
        and not playback_result.errors
        and restore.status == "complete"
        and snapshot_bytes_before == snapshot_bytes_after
        and failure.status == "failed"
        and failure_low_files == ["slice-00000.fslice"]
        and not temporary_files
        and deterministic_slices
        else 1
    )


if __name__ == "__main__":
    raise SystemExit(main())
