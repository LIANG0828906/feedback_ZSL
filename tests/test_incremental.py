from __future__ import annotations

import threading
from pathlib import Path
from typing import Any

import pytest

import haptic_studio.engine as engine_module
from haptic_studio.errors import ErrorCode, HapticError
from haptic_studio.models import DeviceConfig, WaveSequence
from haptic_studio.sampler import CancelToken, compute_full_signal
from haptic_studio.session import HapticSession


def _session(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> HapticSession:
    return HapticSession.create(sequence, config, tmp_path / "session")


def test_envelope_edit_recomputes_only_one_segment_and_transitions(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = _session(tmp_path, sequence, config)
    payload = sequence.to_dict()
    payload["segments"][1]["envelope"] = "sine"
    edited = WaveSequence.from_mapping(payload)
    published = session.recompute(edited)
    full = compute_full_signal(edited, config)
    assert published.recomputed_segments == (1,)
    assert published.reused_segments == (0, 2)
    assert not published.full_recompute
    assert published.signal_hash == full.signal_hash
    assert published.sampling.left_bytes == full.left_bytes
    assert published.sampling.right_bytes == full.right_bytes
    old_signal = session.publication_history()[0].sampling.segment_by_index[1]
    new_signal = published.sampling.segment_by_index[1]
    expected_start = min(old_signal.start_sample, new_signal.start_sample) - 1
    expected_end = max(old_signal.end_sample, new_signal.end_sample) + 1
    assert published.affected_ranges == ((expected_start, expected_end),)
    assert published.recomputed_sample_count == expected_end - expected_start


def test_allocation_change_recomputes_all_segments_but_not_forced_full(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = _session(tmp_path, sequence, config)
    changed = DeviceConfig.from_mapping(
        {**config.to_dict(), "allocation_ratio": [0.7, 0.3]}
    )
    published = session.recompute(sequence, new_config=changed)
    assert published.recomputed_segments == (0, 1, 2)
    assert published.reused_segments == ()
    assert not published.full_recompute
    assert "allocation_changed" in published.reasons
    assert published.affected_ranges != ((0, published.sampling.total_frames),)
    assert published.signal_hash == compute_full_signal(sequence, changed).signal_hash


@pytest.mark.parametrize(
    ("field_name", "value", "reason"),
    [
        ("sample_rate", 2000, "sample_rate_changed"),
        ("bit_depth", 12, "bit_depth_changed"),
        ("limiter", "soft", "limiter_changed"),
    ],
)
def test_global_config_items_conservatively_full_recompute(
    tmp_path: Path,
    sequence: WaveSequence,
    config: DeviceConfig,
    field_name: str,
    value: Any,
    reason: str,
) -> None:
    session = _session(tmp_path, sequence, config)
    changed_payload = config.to_dict()
    changed_payload[field_name] = value
    changed = DeviceConfig.from_mapping(changed_payload)
    published = session.recompute(sequence, new_config=changed)
    assert published.full_recompute
    assert published.full_reason == reason
    assert published.reused_segments == ()
    assert published.recomputed_segments == (0, 1, 2)
    assert published.affected_ranges == ((0, published.sampling.total_frames),)


def test_no_effective_change_reuses_everything(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = _session(tmp_path, sequence, config)
    previous = session.latest_publication
    published = session.recompute(sequence)
    assert published.recomputed_segments == ()
    assert published.reused_segments == (0, 1, 2)
    assert published.affected_ranges == ()
    assert published.recomputed_sample_count == 0
    assert published.signal_hash == previous.signal_hash
    assert published.sampling.left_bytes == previous.sampling.left_bytes


def test_cancel_retains_last_publication(
    tmp_path: Path, sequence: WaveSequence, config: DeviceConfig
) -> None:
    session = _session(tmp_path, sequence, config)
    before = session.latest_publication
    payload = sequence.to_dict()
    payload["segments"][0]["amplitude"] = 0.9
    token = CancelToken()
    token.cancel()
    with pytest.raises(HapticError) as caught:
        session.recompute(WaveSequence.from_mapping(payload), cancellation=token)
    assert caught.value.code == ErrorCode.CANCELLED
    assert session.latest_publication.publication_version == before.publication_version
    assert session.latest_publication.signal_hash == before.signal_hash


def test_mid_compute_cancel_is_cooperative(
    tmp_path: Path,
    sequence: WaveSequence,
    config: DeviceConfig,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    session = _session(tmp_path, sequence, config)
    before = session.latest_publication
    started = threading.Event()
    release = threading.Event()
    original = engine_module.compute_segment_signal

    def slow_compute(*args: Any, **kwargs: Any) -> Any:
        started.set()
        assert release.wait(5)
        return original(*args, **kwargs)

    monkeypatch.setattr(engine_module, "compute_segment_signal", slow_compute)
    payload = sequence.to_dict()
    payload["segments"][0]["envelope"] = "sine"
    edited = WaveSequence.from_mapping(payload)
    token = CancelToken()
    errors: list[Exception] = []

    def run() -> None:
        try:
            session.recompute(edited, cancellation=token)
        except Exception as exc:
            errors.append(exc)

    thread = threading.Thread(target=run)
    thread.start()
    assert started.wait(5)
    token.cancel()
    release.set()
    thread.join(5)
    assert not thread.is_alive()
    assert len(errors) == 1
    assert isinstance(errors[0], HapticError)
    assert errors[0].code == ErrorCode.CANCELLED
    assert session.latest_publication.publication_version == before.publication_version
    assert session.latest_publication.signal_hash == before.signal_hash


def test_second_recompute_returns_busy(
    tmp_path: Path,
    sequence: WaveSequence,
    config: DeviceConfig,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    session = _session(tmp_path, sequence, config)
    started = threading.Event()
    release = threading.Event()
    original = engine_module.compute_segment_signal

    def slow_compute(*args: Any, **kwargs: Any) -> Any:
        started.set()
        assert release.wait(5)
        return original(*args, **kwargs)

    monkeypatch.setattr(engine_module, "compute_segment_signal", slow_compute)
    payload = sequence.to_dict()
    payload["segments"][0]["amplitude"] = 0.9
    edited = WaveSequence.from_mapping(payload)
    errors: list[Exception] = []

    def run_first() -> None:
        try:
            session.recompute(edited)
        except Exception as exc:
            errors.append(exc)

    thread = threading.Thread(target=run_first)
    thread.start()
    assert started.wait(5)
    with pytest.raises(HapticError) as caught:
        session.recompute(edited)
    assert caught.value.code == ErrorCode.BUSY
    release.set()
    thread.join(5)
    assert not thread.is_alive()
    assert errors == []
