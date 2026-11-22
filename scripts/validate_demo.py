"""Run the end-to-end local acceptance suite and write a compact report."""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import textwrap
from dataclasses import replace
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from lcft.budget import DEGRADATION_STAGES, usage_for_stage
from lcft.canonical import atomic_write_json, canonical_bytes
from lcft.engine import incremental_layout, layout_track
from lcft.errors import LCFTError
from lcft.generator import generate_sample
from lcft.loader import load_bundle
from lcft.params import LayoutParams
from lcft.precompute import precompute_track
from lcft.recovery import resume_from_snapshot
from lcft.runner import BatchOptions, run_batch
from lcft.snapshot import SnapshotStore
from lcft.wav import inspect_wav
from lcft.workbench import WorkbenchSession

DEMO = ROOT / "artifacts" / "lcft-demo"
OUT = ROOT / "artifacts" / "lcft-validation"


def main() -> int:
    if not (DEMO / "bundle.json").exists():
        generate_sample(
            DEMO,
            seed=20261122,
            sentence_count=32,
            languages=("zh", "en"),
            channels=1,
            duration_seconds="72",
            description_density_permille=220,
            contention_permille=450,
            read_speed_min=9,
            read_speed_max=16,
        )
    loaded = load_bundle(DEMO / "bundle.json")
    OUT.mkdir(parents=True, exist_ok=True)
    report: dict = {"schema_version": 1, "checks": {}, "evidence": {}}

    full = run_batch(
        DEMO / "bundle.json",
        OUT / "full",
        options=BatchOptions(workers=4, write_snapshots=True),
    )
    report["checks"]["multi_track_publish"] = {
        "ok": full["ok"],
        "published_hashes": full["published_hashes"],
        "baseline_match": full.get("baseline_match"),
        "refresh_count": full["workbench_refresh_count"],
    }
    report["evidence"]["full_tracks"] = "artifacts/lcft-validation/full/tracks"
    report["evidence"]["full_trace"] = "artifacts/lcft-validation/full/trace-log.json"

    single = run_batch(
        DEMO / "bundle.json",
        OUT / "workers-1",
        options=BatchOptions(workers=1, write_snapshots=False),
    )
    report["checks"]["parallel_commit_determinism"] = {
        "ok": (
            single["published_hashes"] == full["published_hashes"]
            and canonical_bytes(single["scheduler_trace"])
            == canonical_bytes(full["scheduler_trace"])
        ),
        "hashes_equal": single["published_hashes"] == full["published_hashes"],
        "trace_bytes_equal": (
            canonical_bytes(single["scheduler_trace"])
            == canonical_bytes(full["scheduler_trace"])
        ),
    }

    minimal = DEGRADATION_STAGES[-1]
    quota = 0
    for language in loaded.languages:
        speech = loaded.normalized.tracks[language]
        descriptions = (
            loaded.normalized.descriptions
            if language == loaded.languages[0]
            else tuple()
        )
        text_bytes = sum(
            len(item.text.encode("utf-8"))
            for item in tuple(speech) + tuple(descriptions)
        )
        usage = usage_for_stage(
            minimal,
            sentence_count=len(speech) + len(descriptions),
            text_bytes=text_bytes,
            stage_index=len(DEGRADATION_STAGES) - 1,
        )
        quota = max(quota, usage.total_bytes)
    degraded = run_batch(
        DEMO / "bundle.json",
        OUT / "degraded",
        options=BatchOptions(
            workers=4,
            job_quota=quota,
            global_quota=quota,
            write_snapshots=False,
        ),
    )
    stages = [
        event["stage"]
        for event in degraded["scheduler_trace"]
        if event["event"] == "budget_stage"
    ]
    report["checks"]["budget_degradation"] = {
        "ok": degraded["ok"]
        and degraded["published_hashes"] == full["published_hashes"]
        and any(stage != "full" for stage in stages),
        "quota_bytes": quota,
        "stages": stages,
        "degraded": any(stage != "full" for stage in stages),
        "hashes_equal_to_full": degraded["published_hashes"] == full["published_hashes"],
    }

    previous = json.loads(
        (OUT / "full" / "tracks" / "en.json").read_text(encoding="utf-8")
    )
    edited_speech = list(loaded.normalized.tracks["en"])
    edited_speech[-1] = replace(edited_speech[-1], text="one two")
    incremental, stats = incremental_layout(
        previous,
        edited_speech,
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=2,
        param_version=2,
    )
    full_relayout = layout_track(
        "speech-en",
        "en",
        edited_speech,
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=2,
        param_version=2,
    )
    report["checks"]["incremental"] = {
        "ok": incremental["track_hash"] == full_relayout["track_hash"],
        "track_hash_equal_to_full": incremental["track_hash"]
        == full_relayout["track_hash"],
        "affected_count": stats["affected_count"],
        "reused_count": stats["reused_count"],
        "affected_ids": stats["affected_ids"],
        "conservative": stats["conservative"],
    }

    corrupt_dir = OUT / "corrupt-snapshot"
    if corrupt_dir.exists():
        shutil.rmtree(corrupt_dir)
    shutil.copytree(OUT / "full" / "snapshots" / "en", corrupt_dir)
    corrupt_path = corrupt_dir / "snapshot-v000001.json"
    corrupt = json.loads(corrupt_path.read_text(encoding="utf-8"))
    corrupt["chain"][2]["hash"] = "broken"
    corrupt_path.write_text(
        json.dumps(corrupt, ensure_ascii=False, sort_keys=True, separators=(",", ":")),
        encoding="utf-8",
    )
    try:
        SnapshotStore(corrupt_dir, "speech-en").load_latest()
        chain_result = {"detected": False}
    except LCFTError as exc:
        chain_result = {
            "detected": exc.code == "E_RECOVERY_INVALID",
            "code": exc.code,
            "field": exc.field,
            "first_broken_link": exc.details.get("first_broken_link"),
        }
    report["checks"]["broken_hash_chain"] = chain_result

    crash_dir = OUT / "crash-snapshots"
    if crash_dir.exists():
        shutil.rmtree(crash_dir)
    base_store = SnapshotStore(crash_dir, "speech-en")
    base_report = layout_track(
        "speech-en",
        "en",
        loaded.normalized.tracks["en"],
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=1,
        param_version=1,
    )
    base_store.save_published(base_report)
    completed = 8
    script = textwrap.dedent(
        f"""
        import time
        from lcft.loader import load_bundle
        from lcft.params import LayoutParams
        from lcft.precompute import precompute_track
        from lcft.snapshot import SnapshotStore
        loaded = load_bundle({str(DEMO / 'bundle.json')!r})
        store = SnapshotStore({str(crash_dir)!r}, 'speech-en')
        base = store.load_latest().report
        speech = loaded.normalized.tracks['en']
        descriptions = loaded.normalized.descriptions
        pre = precompute_track('speech-en', speech[:{completed}], descriptions[:1], LayoutParams(), 48000, workers=2)
        store.save_checkpoint(
            base,
            precomputed_sentences=pre.sentences,
            precomputed_descriptions=pre.descriptions,
            completed_ids=[item.id for item in speech[:{completed}]],
            pending_sentence_ids=[item.id for item in speech[{completed}:]],
        )
        print('checkpointed', flush=True)
        time.sleep(30)
        """
    )
    process = subprocess.Popen(
        [sys.executable, "-c", script],
        cwd=str(ROOT),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    ready = process.stdout.readline().strip() if process.stdout else ""
    if process.poll() is None:
        os.kill(process.pid, signal.SIGKILL)
    process.wait(timeout=5)
    no_exit_report = not (OUT / "exit-report.json").exists()
    resumed = resume_from_snapshot(
        loaded, crash_dir, "en", OUT / "resumed", workers=2
    )
    report["checks"]["forced_termination_recovery"] = {
        "ok": ready == "checkpointed" and resumed["ok"] and no_exit_report,
        "checkpoint_ready": ready == "checkpointed",
        "recovered_from_published_version": resumed[
            "recovered_from_published_version"
        ],
        "reused_count": resumed["reused_count"],
        "recomputed_count": resumed["recomputed_count"],
        "exit_report_promised": False,
        "exit_report_exists": not no_exit_report,
    }
    report["evidence"]["resume_summary"] = "artifacts/lcft-validation/resumed/resume-summary.json"

    surround = OUT / "surround-sample"
    if not (surround / "bundle.json").exists():
        generate_sample(
            surround,
            seed=551,
            sentence_count=2,
            languages=("zh",),
            channels=6,
            duration_seconds="6",
            description_density_permille=0,
            contention_permille=0,
            read_speed_min=10,
            read_speed_max=10,
        )
    surround_info = inspect_wav(surround / "audio.wav")
    surround_batch = run_batch(
        surround / "bundle.json",
        OUT / "surround-delivery",
        options=BatchOptions(workers=2, write_snapshots=False),
    )
    report["checks"]["surround_audio"] = {
        "ok": surround_info.channels == 6
        and surround_info.sample_rate == 48_000
        and surround_info.sample_width == 2
        and surround_batch["ok"],
        "channels": surround_info.channels,
        "sample_rate": surround_info.sample_rate,
        "sample_width": surround_info.sample_width,
        "layout_published": surround_batch["ok"],
    }

    operations = [
        {"type": "set_params", "changes": {"min_display_seconds": "1.1"}},
        {
            "type": "edit_sentence",
            "language": "en",
            "id": loaded.normalized.tracks["en"][-1].id,
            "text": "one two",
        },
    ]

    def workbench(name: str):
        session = WorkbenchSession(DEMO / "bundle.json", OUT / name)
        session.run_operations(
            [],
            workers=4,
            job_quota=16_000_000,
            global_quota=32_000_000,
        )
        session.run_operations(
            operations,
            workers=4,
            job_quota=16_000_000,
            global_quota=32_000_000,
        )
        return session

    workbench_a = workbench("workbench-a")
    workbench_b = workbench("workbench-b")
    workbench_a.export_trace(OUT / "workbench-a" / "trace.json")
    report["checks"]["workbench_determinism"] = {
        "ok": canonical_bytes(workbench_a.trace)
        == canonical_bytes(workbench_b.trace)
        and workbench_a.reports["en"]["track_hash"]
        == workbench_b.reports["en"]["track_hash"],
        "trace_bytes_equal": canonical_bytes(workbench_a.trace)
        == canonical_bytes(workbench_b.trace),
        "track_hash": workbench_a.reports["en"]["track_hash"],
        "refresh_count": workbench_a.refresh,
    }
    report["evidence"]["workbench_trace"] = (
        "artifacts/lcft-validation/workbench-a/trace.json"
    )

    report["all_ok"] = all(
        item.get("ok", item.get("detected", False))
        for item in report["checks"].values()
    )
    atomic_write_json(OUT / "report.json", report)
    print(json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2))
    return 0 if report["all_ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

