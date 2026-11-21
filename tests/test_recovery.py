from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from mcsync.errors import E_RECOVERY_INVALID
from mcsync.recovery import (
    ChannelRange,
    RecoveryJournal,
    contiguous_ranges,
)
from mcsync.resample import resample_channel_range


@pytest.fixture(scope="module")
def recovery_fixture(synthetic, processed, tmp_path_factory):
    root = tmp_path_factory.mktemp("recovery")
    journal = RecoveryJournal(root, processed.report)
    fits = {fit.channel: fit for fit in processed.report.drift_fits}
    output_length = processed.compensated_audio.frame_count
    ranges = contiguous_ranges(
        range(8), output_length=output_length, chunk_length=48000
    )

    def compensate(channel_range: ChannelRange) -> np.ndarray:
        return resample_channel_range(
            synthetic.audio.channel(channel_range.channel),
            fits[channel_range.channel],
            mode=processed.report.compensation.mode,
            output_start=channel_range.start_sample,
            output_end=channel_range.end_sample,
        )

    return root, journal, ranges, compensate, output_length


def test_resume_skips_committed_ranges(recovery_fixture):
    root, journal, ranges, compensate, output_length = recovery_fixture
    for channel_range in ranges[:5]:
        journal.commit_range(channel_range, compensate(channel_range))
    restarted = RecoveryJournal(root, journal.report)
    resumed = restarted.resume(
        ranges,
        channels=range(8),
        output_length=output_length,
        compensate_range=compensate,
    )
    assert len(resumed.skipped_committed_ranges) == 5
    assert len(resumed.written_ranges) == len(ranges) - 5
    assert resumed.matrix.shape == (8, output_length)
    assert resumed.state.last_sequence == len(ranges) - 1


def test_duplicate_commit_is_rejected(tmp_path, processed):
    journal = RecoveryJournal(tmp_path, processed.report)
    channel_range = ChannelRange(0, 0, 32)
    journal.commit_range(channel_range, np.zeros(32))
    with pytest.raises(Exception) as raised:
        journal.commit_range(channel_range, np.zeros(32))
    assert raised.value.code == E_RECOVERY_INVALID


def test_broken_hash_chain_aborts(tmp_path, processed):
    journal = RecoveryJournal(tmp_path, processed.report)
    channel_range = ChannelRange(0, 0, 32)
    journal.commit_range(channel_range, np.zeros(32))
    second = ChannelRange(0, 32, 64)
    journal.commit_range(second, np.ones(32))
    path = sorted(tmp_path.glob("commit_*.json"))[1]
    record = json.loads(path.read_text(encoding="utf-8"))
    record["prev_hash"] = "f" * 64
    path.write_text(json.dumps(record), encoding="utf-8")
    with pytest.raises(Exception) as raised:
        RecoveryJournal(tmp_path, processed.report).load_state()
    assert raised.value.code == E_RECOVERY_INVALID


def test_missing_required_field_aborts(tmp_path, processed):
    journal = RecoveryJournal(tmp_path, processed.report)
    journal.commit_range(ChannelRange(0, 0, 32), np.zeros(32))
    path = sorted(tmp_path.glob("commit_*.json"))[0]
    record = json.loads(path.read_text(encoding="utf-8"))
    del record["compensation"]
    path.write_text(json.dumps(record), encoding="utf-8")
    with pytest.raises(Exception) as raised:
        RecoveryJournal(tmp_path, processed.report).load_state()
    assert raised.value.code == E_RECOVERY_INVALID

