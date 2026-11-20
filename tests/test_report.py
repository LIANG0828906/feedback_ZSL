from __future__ import annotations

import json

import pytest

from broadcast_loudness.errors import ReportMismatchError
from broadcast_loudness.report import load_report, save_report
from broadcast_loudness.session import SessionManager


def test_atomic_report_round_trip_and_checkpoint_recovery(make_audio, tmp_path) -> None:
    audio, params = make_audio(duration=4.0)
    session = SessionManager().open_session(audio, params)
    session.advance(2 * 48_000)
    snapshot = session.snapshot()
    path = tmp_path / "report.json"
    digest = save_report(path, audio, snapshot)
    loaded = load_report(path, audio=audio)
    assert len(loaded.blocks) == snapshot.committed_count
    assert loaded.chain_head == loaded.document["hash_chain"]["head"]
    assert not list(tmp_path.glob("*.tmp"))
    assert len(digest) == 64


def test_tampered_hash_chain_is_rejected_without_overwrite(
    make_audio, tmp_path
) -> None:
    audio, params = make_audio(duration=3.0)
    session = SessionManager().open_session(audio, params)
    session.advance(audio.frame_count)
    path = tmp_path / "report.json"
    save_report(path, audio, session.snapshot())
    document = json.loads(path.read_text(encoding="utf-8"))
    document["blocks"][2]["momentary_lufs"] = -1.0
    path.write_text(
        json.dumps(document, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    before = path.read_bytes()
    with pytest.raises(ReportMismatchError) as error:
        load_report(path, audio=audio)
    assert error.value.code == "E_REPORT_MISMATCH"
    assert path.read_bytes() == before


def test_version_and_source_mismatch_are_rejected(make_audio, tmp_path) -> None:
    audio, params = make_audio(duration=3.0)
    session = SessionManager().open_session(audio, params)
    session.advance(48_000)
    path = tmp_path / "report.json"
    save_report(path, audio, session.snapshot())
    document = json.loads(path.read_text(encoding="utf-8"))
    document["report_version"] = 999
    path.write_text(json.dumps(document) + "\n", encoding="utf-8")
    with pytest.raises(ReportMismatchError):
        load_report(path)

    audio2, _ = make_audio(name="other.wav", seed=99, duration=3.0)
    save_report(path, audio, session.snapshot())
    with pytest.raises(ReportMismatchError):
        load_report(path, audio=audio2)
