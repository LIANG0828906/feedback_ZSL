from __future__ import annotations

import copy

import pytest

from lcft.engine import layout_track
from lcft.errors import LCFTError
from lcft.hashchain import validate_chain
from lcft.params import LayoutParams
from lcft.timebase import unify_timebase

from conftest import descriptor_variant


def test_layout_report_has_stable_chain_and_rules(sample_dir, sample_raw):
    normalized = unify_timebase(sample_raw, base_dir=sample_dir)
    report = layout_track(
        "speech-en",
        "en",
        normalized.tracks["en"],
        normalized.descriptions,
        normalized,
        LayoutParams(),
    )
    validate_chain(report["chain"], report)
    assert report["track_hash"]
    assert all(row["content_hash"] for row in report["sentences"])
    assert all(row["verdict"] for row in report["sentences"])
    again = layout_track(
        "speech-en",
        "en",
        normalized.tracks["en"],
        normalized.descriptions,
        normalized,
        LayoutParams(),
    )
    assert again["track_hash"] == report["track_hash"]
    assert again["sentences"] == report["sentences"]


def test_read_speed_extension_is_explained(descriptor_dir, descriptor_raw):
    raw = copy.deepcopy(descriptor_raw)
    raw["tracks"]["en"]["sentences"][0]["text"] = (
        "one two three four five six seven eight nine ten"
    )
    raw["tracks"]["en"]["sentences"][0]["max_read_speed"] = 7
    raw["tracks"]["en"]["sentences"][0]["end"] = (
        raw["tracks"]["en"]["sentences"][0]["start"] + 48_000
    )
    normalized = unify_timebase(raw, base_dir=descriptor_dir)
    report = layout_track(
        "speech-en",
        "en",
        normalized.tracks["en"],
        (),
        normalized,
        LayoutParams(),
    )
    first = report["sentences"][0]
    assert first["duration_ticks"] >= first["ideal_ticks"]
    assert first["effects"]
    assert "stretched" in first["effects"]
    assert "R1_READ_SPEED" in first["rules"]
    assert any(reason["rule"] == "R1_READ_SPEED" for reason in first["reasons"])


def test_high_priority_description_trims_speech_and_records_contention(
    descriptor_dir, descriptor_raw
):
    raw = descriptor_variant(
        descriptor_raw,
        description_text="one two three four five",
        description_priority=5,
        speech_priority=3,
    )
    normalized = unify_timebase(raw, base_dir=descriptor_dir)
    report = layout_track(
        "speech-en",
        "en",
        normalized.tracks["en"],
        normalized.descriptions,
        normalized,
        LayoutParams(),
    )
    speech = report["sentences"][0]
    description = report["descriptions"][0]
    assert description["verdict"] == "placed"
    assert speech["duration_ticks"] < speech["original_duration_ticks"]
    assert report["trims"]
    trim = report["trims"][0]
    assert trim["target_id"] == speech["id"]
    assert trim["participants"] == ["desc-test", speech["id"]]
    assert trim["trimmed_ticks"] > 0
    assert report["contention_edges"]


def test_lower_priority_description_is_rejected_without_aborting(
    descriptor_dir, descriptor_raw
):
    raw = descriptor_variant(
        descriptor_raw,
        description_text="one two three four five",
        description_priority=1,
        speech_priority=3,
    )
    normalized = unify_timebase(raw, base_dir=descriptor_dir)
    report = layout_track(
        "speech-en",
        "en",
        normalized.tracks["en"],
        normalized.descriptions,
        normalized,
        LayoutParams(),
    )
    assert report["descriptions"][0]["verdict"] == "rejected"
    assert report["trims"][0]["rule"] == "D2_PRIORITY_LOSER"
    validate_chain(report["chain"], report)


def test_equal_priority_description_conflict_lists_participants(
    descriptor_dir, descriptor_raw
):
    raw = descriptor_variant(
        descriptor_raw,
        description_text="one two three four five",
        description_priority=3,
        speech_priority=3,
    )
    normalized = unify_timebase(raw, base_dir=descriptor_dir)
    with pytest.raises(LCFTError) as caught:
        layout_track(
            "speech-en",
            "en",
            normalized.tracks["en"],
            normalized.descriptions,
            normalized,
            LayoutParams(),
        )
    assert caught.value.code == "E_DESC_CONFLICT"
    assert caught.value.field == "descriptions.desc-test"
    participants = caught.value.details["participants"]
    assert "desc-test" in participants
    assert any(item.startswith("en-s") for item in participants)

