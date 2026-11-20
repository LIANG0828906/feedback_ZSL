from __future__ import annotations

import json
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest

from conftest import recipe_v1, recipe_v2, recipe_v3
from game_audio_restorer.errors import BusinessError, ErrorCode
from game_audio_restorer.restore import RestorationEngine, export_existing
from game_audio_restorer.snapshot import load_and_validate
from game_audio_restorer.wav_io import read_wav


def test_complete_run_report_trace_and_hashes(tmp_path: Path, stereo_wav: Path) -> None:
    work = tmp_path / "work"
    recipe = recipe_v3()
    result = RestorationEngine(stereo_wav, recipe, work).run()
    assert result.status == "completed"
    assert result.committed_segments == 3
    assert Path(result.output_wav).is_file()
    report = json.loads(Path(result.report_path).read_text("utf-8"))
    assert report["recipe"]["version"] == 3
    assert len(report["segments"]) == 3
    assert report["recipe"]["lost_fields"] == []
    assert report["output"]["sha256"]
    assert report["snapshot"]["hash_chain_head"] == result.hash_chain_head
    trace_lines = Path(result.trace_path).read_text("utf-8").splitlines()
    assert len(trace_lines) == 4
    first = json.loads(trace_lines[0])
    assert first["field_mapping"]
    for line in trace_lines[1:]:
        row = json.loads(line)
        assert row["content_sha256"]
        assert row["segment"] in (0, 1, 2)
    output = read_wav(result.output_wav)
    assert output.channels == 2
    assert output.frames == 38400
    assert not list(work.rglob("*.tmp"))
    load_and_validate(
        work,
        expected_source_sha256=read_wav(stereo_wav).sha256,
        expected_recipe_hash=recipe.hash(),
    )


def test_cancel_keeps_committed_segment_and_resume_does_not_rewrite(
    tmp_path: Path, stereo_wav: Path
) -> None:
    work = tmp_path / "work"
    recipe = recipe_v2()
    cancel = threading.Event()

    def stop_after_first(_entry: dict) -> None:
        cancel.set()

    partial = RestorationEngine(stereo_wav, recipe, work).run(
        cancel_event=cancel, on_segment=stop_after_first
    )
    assert partial.status == "cancelled"
    assert partial.committed_segments == 1
    assert partial.cancelled_at_segment == 1
    assert not (work / "restored.wav").exists()
    assert not (work / "report.json").exists()
    segment_file = work / "segments" / "000000.wav"
    before = (segment_file.stat().st_mtime_ns, segment_file.read_bytes())

    completed = RestorationEngine(stereo_wav, recipe, work).run()
    assert completed.status == "completed"
    assert completed.committed_segments == 3
    after = (segment_file.stat().st_mtime_ns, segment_file.read_bytes())
    assert before == after
    report = json.loads(Path(completed.report_path).read_text("utf-8"))
    assert [entry["index"] for entry in report["segments"]] == [0, 1, 2]


def test_corrupt_segment_reports_first_break(
    tmp_path: Path, stereo_wav: Path
) -> None:
    work = tmp_path / "work"
    recipe = recipe_v2()
    cancel = threading.Event()

    def stop_after_second(entry: dict) -> None:
        if entry["index"] == 1:
            cancel.set()

    RestorationEngine(stereo_wav, recipe, work).run(
        cancel_event=cancel, on_segment=stop_after_second
    )
    segment = work / "segments" / "000001.wav"
    raw = bytearray(segment.read_bytes())
    raw[-1] ^= 0x01
    segment.write_bytes(raw)
    with pytest.raises(BusinessError) as caught:
        RestorationEngine(stereo_wav, recipe, work).run()
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
    assert caught.value.details["first_break_segment"] == 1
    assert caught.value.details["location"]["field"] == "sha256"


def test_broken_chain_reports_first_break(tmp_path: Path, stereo_wav: Path) -> None:
    work = tmp_path / "work"
    recipe = recipe_v2()
    cancel = threading.Event()

    def stop_after_second(entry: dict) -> None:
        if entry["index"] == 1:
            cancel.set()

    RestorationEngine(stereo_wav, recipe, work).run(
        cancel_event=cancel, on_segment=stop_after_second
    )
    manifest_path = work / "snapshot.json"
    manifest = json.loads(manifest_path.read_text("utf-8"))
    manifest["segments"][1]["chain_hash"] = "0" * 64
    manifest_path.write_text(
        json.dumps(manifest, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    with pytest.raises(BusinessError) as caught:
        RestorationEngine(stereo_wav, recipe, work).run()
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
    assert caught.value.details["first_break_segment"] == 1


def test_stale_recipe_is_rejected_on_resume_and_export(
    tmp_path: Path, stereo_wav: Path
) -> None:
    work = tmp_path / "work"
    cancel = threading.Event()
    RestorationEngine(stereo_wav, recipe_v1(), work).run(
        cancel_event=cancel, on_segment=lambda _entry: cancel.set()
    )
    with pytest.raises(BusinessError) as caught:
        RestorationEngine(stereo_wav, recipe_v2(), work).run()
    assert caught.value.code == ErrorCode.VERSION_STALE

    engine = RestorationEngine(stereo_wav, recipe_v1(), work)
    # Finish with the original recipe, then require a different recipe at export.
    engine.run()
    with pytest.raises(BusinessError) as caught_export:
        export_existing(stereo_wav, recipe_v3(), work)
    assert caught_export.value.code == ErrorCode.VERSION_STALE


def test_same_wav_returns_busy(tmp_path: Path, stereo_wav: Path) -> None:
    work1 = tmp_path / "work1"
    work2 = tmp_path / "work2"
    entered = threading.Event()
    release = threading.Event()
    outcomes: list[object] = []

    def block_after_first(_entry: dict) -> None:
        entered.set()
        assert release.wait(10)

    def first_run() -> None:
        outcomes.append(
            RestorationEngine(stereo_wav, recipe_v2(), work1).run(
                on_segment=block_after_first
            )
        )

    thread = threading.Thread(target=first_run)
    thread.start()
    assert entered.wait(10)
    try:
        with pytest.raises(BusinessError) as caught:
            RestorationEngine(stereo_wav, recipe_v2(), work2).run()
        assert caught.value.code == ErrorCode.BUSY
    finally:
        release.set()
        thread.join(15)
    assert not thread.is_alive()
    assert outcomes and outcomes[0].status == "completed"


def test_force_kill_then_disk_snapshot_resume(
    tmp_path: Path, stereo_wav: Path
) -> None:
    work = tmp_path / "force-work"
    marker = tmp_path / "committed.marker"
    recipe_path = tmp_path / "recipe.json"
    recipe = recipe_v2()
    recipe_path.write_text(
        json.dumps(recipe.semantic_dict(), ensure_ascii=False), encoding="utf-8"
    )
    helper = Path(__file__).parent / "helpers" / "force_kill_worker.py"
    process = subprocess.Popen(
        [
            sys.executable,
            str(helper),
            "--wav",
            str(stereo_wav),
            "--recipe",
            str(recipe_path),
            "--work",
            str(work),
            "--marker",
            str(marker),
        ],
        cwd=str(Path(__file__).resolve().parents[1]),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    try:
        deadline = time.monotonic() + 20
        while not marker.exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.02)
        assert marker.exists(), process.stderr.read().decode("utf-8", errors="replace")
        process.kill()
        process.wait(timeout=10)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=10)

    assert process.returncode != 0
    assert not (work / "report.json").exists()
    partial = load_and_validate(
        work,
        expected_source_sha256=read_wav(stereo_wav).sha256,
        expected_recipe_hash=recipe.hash(),
    )
    assert len(partial["segments"]) == 1
    committed_path = work / partial["segments"][0]["path"]
    before = committed_path.read_bytes()

    resumed = RestorationEngine(stereo_wav, recipe, work).run()
    assert resumed.status == "completed"
    assert resumed.committed_segments == 3
    assert committed_path.read_bytes() == before
    assert Path(resumed.report_path).is_file()


def test_missing_snapshot_field_locates_segment(
    tmp_path: Path, stereo_wav: Path
) -> None:
    work = tmp_path / "work"
    recipe = recipe_v2()
    cancel = threading.Event()

    def stop_after_first(entry: dict) -> None:
        if entry["index"] == 0:
            cancel.set()

    RestorationEngine(stereo_wav, recipe, work).run(
        cancel_event=cancel, on_segment=stop_after_first
    )
    manifest_path = work / "snapshot.json"
    manifest = json.loads(manifest_path.read_text("utf-8"))
    del manifest["segments"][0]["metrics"]
    manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    with pytest.raises(BusinessError) as caught:
        RestorationEngine(stereo_wav, recipe, work).run()
    assert caught.value.code == ErrorCode.RECOVERY_INVALID
    assert caught.value.details["first_break_segment"] == 0
    assert caught.value.details["location"]["field"] == "metrics"
