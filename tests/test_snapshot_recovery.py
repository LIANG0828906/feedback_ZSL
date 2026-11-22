from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import textwrap

import pytest

from lcft.engine import layout_track
from lcft.errors import LCFTError
from lcft.loader import load_bundle
from lcft.params import LayoutParams
from lcft.precompute import precompute_track
from lcft.recovery import resume_from_snapshot
from lcft.snapshot import SnapshotStore


def _base_report(loaded):
    return layout_track(
        "speech-en",
        "en",
        loaded.normalized.tracks["en"],
        loaded.normalized.descriptions,
        loaded.normalized,
        LayoutParams(),
        published_version=1,
        param_version=1,
    )


def test_snapshot_round_trip(sample_bundle_path, tmp_path):
    loaded = load_bundle(sample_bundle_path)
    report = _base_report(loaded)
    store = SnapshotStore(tmp_path / "snapshots", "speech-en")
    store.save_published(report)
    recovered = store.load_latest()
    assert recovered.checkpoint is False
    assert recovered.report["track_hash"] == report["track_hash"]
    assert recovered.report["sentences"] == report["sentences"]


def test_broken_hash_chain_reports_first_broken_sentence(
    sample_bundle_path, tmp_path
):
    loaded = load_bundle(sample_bundle_path)
    report = _base_report(loaded)
    store = SnapshotStore(tmp_path / "snapshots", "speech-en")
    path = store.save_published(report)
    snapshot = json.loads(path.read_text(encoding="utf-8"))
    broken_index = 3
    snapshot["chain"][broken_index]["hash"] = "broken"
    path.write_text(json.dumps(snapshot), encoding="utf-8")
    with pytest.raises(LCFTError) as caught:
        store.load_latest()
    assert caught.value.code == "E_RECOVERY_INVALID"
    assert caught.value.field == f"snapshot.chain[{broken_index}].hash"
    assert caught.value.details["link_index"] == broken_index
    assert (
        caught.value.details["first_broken_link"]["id"]
        == report["sentences"][broken_index]["id"]
    )


def test_checkpoint_reuses_completed_precompute_rows(
    sample_bundle_path, tmp_path
):
    loaded = load_bundle(sample_bundle_path)
    report = _base_report(loaded)
    speech = loaded.normalized.tracks["en"]
    descriptions = loaded.normalized.descriptions
    store = SnapshotStore(tmp_path / "snapshots", "speech-en")
    store.save_published(report)
    completed_count = 5
    precomputed = precompute_track(
        "speech-en",
        speech[:completed_count],
        descriptions[:1],
        LayoutParams(),
        48_000,
        workers=4,
    )
    store.save_checkpoint(
        report,
        precomputed_sentences=precomputed.sentences,
        precomputed_descriptions=precomputed.descriptions,
        completed_ids=[item.id for item in speech[:completed_count]],
        pending_sentence_ids=[item.id for item in speech[completed_count:]],
    )
    state = store.load_latest()
    assert state.checkpoint is True
    assert state.pending_sentence_ids == [
        item.id for item in speech[completed_count:]
    ]
    resumed = resume_from_snapshot(
        loaded,
        tmp_path / "snapshots",
        "en",
        tmp_path / "resume",
        workers=4,
    )
    assert resumed["ok"] is True
    assert resumed["recomputed_count"] == (
        len(speech) - completed_count
        + len(descriptions)
        - 1
    )
    assert resumed["reused_count"] == completed_count + 1
    assert resumed["exit_report_from_killed_process"] is False


@pytest.mark.skipif(not hasattr(signal, "SIGKILL"), reason="SIGKILL unavailable")
def test_forced_termination_recovers_from_disk_without_exit_report(
    sample_bundle_path, tmp_path
):
    loaded = load_bundle(sample_bundle_path)
    report = _base_report(loaded)
    snapshot_dir = tmp_path / "snapshots"
    store = SnapshotStore(snapshot_dir, "speech-en")
    store.save_published(report)
    speech = loaded.normalized.tracks["en"]
    descriptions = loaded.normalized.descriptions
    completed = 4
    script = textwrap.dedent(
        f"""
        import os, signal, time
        from lcft.loader import load_bundle
        from lcft.precompute import precompute_track
        from lcft.snapshot import SnapshotStore
        from lcft.params import LayoutParams
        loaded = load_bundle({str(sample_bundle_path)!r})
        store = SnapshotStore({str(snapshot_dir)!r}, 'speech-en')
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
        cwd=os.getcwd(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        line = process.stdout.readline().strip() if process.stdout else ""
        assert line == "checkpointed"
        os.kill(process.pid, signal.SIGKILL)
        process.wait(timeout=5)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
    assert not (tmp_path / "exit-report.json").exists()
    state = SnapshotStore(snapshot_dir, "speech-en").load_latest()
    assert state.checkpoint is True
    resumed = resume_from_snapshot(
        loaded, snapshot_dir, "en", tmp_path / "resumed", workers=2
    )
    assert resumed["ok"] is True
    assert resumed["checkpoint"] is True
    assert resumed["exit_report_from_killed_process"] is False

