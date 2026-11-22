from __future__ import annotations

import json
import threading
import time

import pytest

from lcft.budget import BudgetController, DEGRADATION_STAGES, usage_for_stage
from lcft.canonical import canonical_bytes
from lcft.errors import LCFTError
from lcft.loader import load_bundle
from lcft.params import LayoutParams
from lcft.precompute import GLOBAL_TASK_REGISTRY, TaskRegistry, precompute_track
from lcft.runner import BatchOptions, run_batch


def test_precompute_is_byte_identical_at_any_parallelism(sample_bundle_path):
    loaded = load_bundle(sample_bundle_path)
    speech = loaded.normalized.tracks["en"]
    descriptions = loaded.normalized.descriptions
    outputs = []
    for workers in (1, 2, 8):
        result = precompute_track(
            "speech-en",
            speech,
            descriptions,
            LayoutParams(),
            48_000,
            workers=workers,
        )
        outputs.append(
            canonical_bytes(
                {
                    "sentences": result.sentences,
                    "descriptions": result.descriptions,
                }
            )
        )
    assert outputs[0] == outputs[1] == outputs[2]


def test_duplicate_track_start_returns_busy():
    registry = TaskRegistry()
    registry.acquire("speech-en", "job-1")
    with pytest.raises(LCFTError) as caught:
        registry.acquire("speech-en", "job-2")
    assert caught.value.code == "E_BUSY"
    assert caught.value.field == "track_id"
    registry.release("speech-en", "job-1")
    registry.acquire("speech-en", "job-3")


def test_batch_uses_process_global_busy_registry(sample_bundle_path, tmp_path):
    GLOBAL_TASK_REGISTRY.acquire("speech-en", "external-job")
    try:
        summary = run_batch(
            sample_bundle_path,
            tmp_path / "busy",
            options=BatchOptions(workers=2, write_snapshots=False),
        )
    finally:
        GLOBAL_TASK_REGISTRY.release("speech-en", "external-job")
    assert summary["ok"] is False
    assert summary["error"]["code"] == "E_BUSY"


def test_batch_is_identical_for_different_worker_counts(
    sample_bundle_path, tmp_path
):
    first = run_batch(
        sample_bundle_path,
        tmp_path / "w1",
        options=BatchOptions(workers=1, write_snapshots=False),
    )
    second = run_batch(
        sample_bundle_path,
        tmp_path / "w8",
        options=BatchOptions(workers=8, write_snapshots=False),
    )
    assert first["ok"] and second["ok"]
    assert first["published_hashes"] == second["published_hashes"]
    assert canonical_bytes(first["scheduler_trace"]) == canonical_bytes(
        second["scheduler_trace"]
    )


def test_budget_degradation_changes_stage_not_layout_hash(
    sample_bundle_path, tmp_path, sample_raw
):
    loaded = load_bundle(sample_bundle_path)
    language = loaded.languages[0]
    speech = loaded.normalized.tracks[language]
    descriptions = loaded.normalized.descriptions
    text_bytes = sum(
        len(item.text.encode("utf-8"))
        for item in tuple(speech) + tuple(descriptions)
    )
    minimal = DEGRADATION_STAGES[-1]
    minimal_index = len(DEGRADATION_STAGES) - 1
    quota = 0
    for language in loaded.languages:
        track_speech = loaded.normalized.tracks[language]
        track_descriptions = (
            loaded.normalized.descriptions
            if language == loaded.languages[0]
            else tuple()
        )
        track_text_bytes = sum(
            len(item.text.encode("utf-8"))
            for item in tuple(track_speech) + tuple(track_descriptions)
        )
        usage = usage_for_stage(
            minimal,
            sentence_count=len(track_speech) + len(track_descriptions),
            text_bytes=track_text_bytes,
            stage_index=minimal_index,
        )
        quota = max(quota, usage.total_bytes)
    baseline = run_batch(
        sample_bundle_path,
        tmp_path / "full",
        options=BatchOptions(workers=2, write_snapshots=False),
    )
    degraded = run_batch(
        sample_bundle_path,
        tmp_path / "compact",
        options=BatchOptions(
            workers=2,
            job_quota=quota,
            global_quota=quota,
            write_snapshots=False,
        ),
    )
    assert degraded["ok"]
    stage_events = [
        event for event in degraded["scheduler_trace"] if event["event"] == "budget_stage"
    ]
    assert stage_events
    assert all(event["stage"] != "full" for event in stage_events)
    assert degraded["published_hashes"] == baseline["published_hashes"]


def test_rejected_budget_does_not_touch_published_track(
    sample_bundle_path, tmp_path
):
    out = tmp_path / "published"
    first = run_batch(
        sample_bundle_path,
        out,
        options=BatchOptions(workers=2, write_snapshots=True),
    )
    assert first["ok"]
    language = first["languages"][0]
    report_path = out / "tracks" / f"{language}.json"
    before = report_path.read_bytes()
    rejection = run_batch(
        sample_bundle_path,
        out,
        options=BatchOptions(
            workers=2,
            job_quota=1_000,
            global_quota=1_000,
            write_snapshots=True,
        ),
    )
    assert rejection["ok"] is False
    assert rejection["error"]["code"] == "E_BUDGET_EXCEEDED"
    assert report_path.read_bytes() == before
    assert rejection["published_unchanged_after_rejection"] is True


def test_global_budget_waits_then_admits():
    usage = usage_for_stage(
        DEGRADATION_STAGES[0],
        sentence_count=1,
        text_bytes=0,
        stage_index=0,
    )
    controller = BudgetController(usage.total_bytes + 1)
    controller.acquire("first", usage)
    done = threading.Event()

    def second_acquire():
        controller.acquire("second", usage)
        controller.release("second", usage)
        done.set()

    thread = threading.Thread(target=second_acquire)
    thread.start()
    for _ in range(100):
        if any(item["event"] == "queued" for item in controller.ledger):
            break
        time.sleep(0.005)
    assert any(item["event"] == "queued" and item["job"] == "second" for item in controller.ledger)
    controller.release("first", usage)
    thread.join(timeout=2)
    assert done.is_set()
    assert any(item["event"] == "acquired" and item["job"] == "second" for item in controller.ledger)

