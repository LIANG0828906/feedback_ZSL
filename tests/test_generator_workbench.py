from __future__ import annotations

import json

from lcft.canonical import canonical_bytes
from lcft.generator import generate_sample
from lcft.loader import load_bundle
from lcft.workbench import WorkbenchSession


def test_sample_generator_is_byte_deterministic(tmp_path):
    first = generate_sample(
        tmp_path / "a",
        seed=4242,
        sentence_count=6,
        languages=("zh", "en"),
        channels=1,
        duration_seconds="18",
        description_density_permille=300,
        contention_permille=600,
    )
    second = generate_sample(
        tmp_path / "b",
        seed=4242,
        sentence_count=6,
        languages=("zh", "en"),
        channels=1,
        duration_seconds="18",
        description_density_permille=300,
        contention_permille=600,
    )
    assert first == second
    for name in ("bundle.json", "audio.wav", "frames.bin", "manifest.json"):
        assert (tmp_path / "a" / name).read_bytes() == (
            tmp_path / "b" / name
        ).read_bytes()


def test_workbench_trace_is_byte_identical_for_same_operations(
    sample_bundle_path, tmp_path
):
    loaded = load_bundle(sample_bundle_path)
    description_id = loaded.normalized.descriptions[0].id
    operations = [
        {"type": "set_params", "changes": {"min_display_seconds": "1.1"}},
        {
            "type": "edit_sentence",
            "language": "en",
            "id": "en-s0017",
            "text": "one two",
        },
        {
            "type": "edit_description",
            "id": description_id,
            "text": "a b",
        },
    ]

    def run(name):
        session = WorkbenchSession(
            sample_bundle_path, tmp_path / name
        )
        initial = session.run_operations(
            [],
            workers=4,
            job_quota=16_000_000,
            global_quota=32_000_000,
        )
        assert initial["ok"]
        edited = session.run_operations(
            operations,
            workers=4,
            job_quota=16_000_000,
            global_quota=32_000_000,
        )
        assert edited["ok"]
        return session

    first = run("first")
    second = run("second")
    assert canonical_bytes(first.trace) == canonical_bytes(second.trace)
    assert first.reports["en"]["track_hash"] == second.reports["en"]["track_hash"]
    events = [event["event"] for event in first.trace]
    assert "sentence_conclusion" in events
    assert "incremental_published" in events
    published = [
        event
        for event in first.trace
        if event["event"] in ("published", "incremental_published")
    ]
    for event in published:
        assert "degradation_stage" in event
        assert "affected" in event
        assert "reused" in event
        assert "timecodes" in event
        assert "track_hash" in event
    conclusion = next(
        event for event in first.trace if event["event"] == "sentence_conclusion"
    )
    assert conclusion["sentence_id"]
    assert conclusion["start_timecode"]
    assert conclusion["content_hash"]
    assert first.refresh == second.refresh
    assert first.refresh == len(first.trace)


def test_workbench_cooperative_cancel_publishes_nothing(
    sample_bundle_path, tmp_path
):
    session = WorkbenchSession(sample_bundle_path, tmp_path / "cancelled")
    session.cancel()
    result = session.run_operations(
        [],
        workers=4,
        job_quota=16_000_000,
        global_quota=32_000_000,
    )
    assert result["ok"] is False
    assert result["cancelled"] is True
    assert session.reports == {}
    assert not (tmp_path / "cancelled" / "tracks").exists()

