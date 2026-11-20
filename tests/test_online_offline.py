from __future__ import annotations

import pytest

from broadcast_loudness.analyzer import analyze_audio, compare_online_offline
from broadcast_loudness.errors import LufsDivergenceError
from broadcast_loudness.session import SessionManager


def test_online_matches_offline_for_any_refresh_cadence(make_audio) -> None:
    audio, params = make_audio(duration=4.5)
    manager = SessionManager()
    session = manager.open_session(audio, params)
    session.play()
    refresh_versions = set()
    for _ in range(60):
        session.advance(4_800)
        if _ % 7 == 0:
            refresh_versions.add(session.view().snapshot.version)
        if _ % 3 == 0:
            session.view()
    session.advance(audio.frame_count - session.position_sample)
    final = session.snapshot()
    offline = analyze_audio(audio, params)
    result = compare_online_offline(final.blocks, offline.blocks)
    assert result["compared_blocks"] == len(offline.blocks)
    assert result["max_abs_delta"] == 0.0
    assert refresh_versions
    session.close()


def test_divergence_reports_first_block_and_both_values(make_audio) -> None:
    audio, params = make_audio(duration=3.0)
    from broadcast_loudness.session import SessionManager as Manager

    session = Manager().open_session(audio, params)
    session.advance(audio.frame_count)
    offline = analyze_audio(audio, params)
    tampered = list(session.snapshot().blocks)
    from dataclasses import replace

    tampered[4] = replace(tampered[4], short_lufs=-3.0)
    with pytest.raises(LufsDivergenceError) as error:
        compare_online_offline(tampered, offline.blocks)
    assert error.value.code == "E_LUFS_DIVERGENCE"
    assert error.value.details["block_index"] == 4
    assert error.value.details["online"] == -3.0
    assert error.value.details["offline"] != -3.0

    extra = offline.blocks + (
        replace(offline.blocks[-1], index=999, sync_marker="extra"),
    )
    with pytest.raises(LufsDivergenceError) as extra_error:
        compare_online_offline(extra, offline.blocks)
    assert extra_error.value.details["offline"] is None
