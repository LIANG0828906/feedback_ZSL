from __future__ import annotations

from broadcast_loudness.session import SessionManager


def test_incremental_parameter_changes_and_immutable_old_snapshot(make_audio) -> None:
    audio, params = make_audio(duration=5.0)
    session = SessionManager().open_session(audio, params)
    session.advance(4 * 48_000)
    first = session.snapshot()
    first_blocks = first.blocks
    block_count = len(first.blocks)
    assert 0 < block_count

    target_changed = session.change_parameters(target_lufs=-20.0)
    assert target_changed.affected_blocks == ()
    assert target_changed.cache_stats["recomputed_measurement_blocks"] == 0
    assert target_changed.blocks == first_blocks
    assert all(old is new for old, new in zip(first.blocks, target_changed.blocks))

    peak_changed = session.change_parameters(true_peak_limit_dbtp=-12.0)
    assert peak_changed.cache_stats["recomputed_measurement_blocks"] == 0
    assert 0 < len(peak_changed.affected_blocks) < block_count

    gate_changed = session.change_parameters(absolute_gate_lufs=-14.5)
    assert gate_changed.cache_stats["recomputed_measurement_blocks"] == 0
    assert gate_changed.affected_blocks
    assert gate_changed.affected_blocks == tuple(
        range(gate_changed.affected_blocks[0], block_count)
    )

    weights = tuple(0.8 if index != 3 else 0.0 for index in range(6))
    mapping_changed = session.change_parameters(weights=weights)
    assert len(mapping_changed.affected_blocks) == block_count
    assert mapping_changed.cache_stats["recomputed_measurement_blocks"] == block_count

    assert first.parameter_version == 1
    assert first.blocks[0].parameter_version == 1
    assert first.params.target_lufs == -23.0
    assert first_blocks[0].momentary_lufs == first.blocks[0].momentary_lufs
    assert session.snapshot().version > mapping_changed.version - 1
    session.close()


def test_published_old_snapshot_never_mutated(make_audio) -> None:
    audio, params = make_audio(duration=3.0)
    session = SessionManager().open_session(audio, params)
    session.advance(2 * 48_000)
    old = session.snapshot()
    old_repr = repr(old)
    session.change_parameters(target_lufs=-18.0)
    session.change_parameters(relative_gate_lu=-9.0)
    assert repr(old) == old_repr
