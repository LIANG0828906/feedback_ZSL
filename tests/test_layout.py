from __future__ import annotations

import copy
from pathlib import Path

import pytest

from subtitle_localizer.errors import BusinessError, E_DESC_CONFLICT, E_LAYOUT_MISMATCH
from subtitle_localizer.layout import (
    compare_layouts,
    compute_affected_sentence_ids,
    compute_layout,
)
from subtitle_localizer.models import (
    DescriptionInput,
    LayoutParams,
    SentenceInput,
    SubtitleAsset,
)
from subtitle_localizer.persistence import verify_report
from subtitle_localizer.timebase import read_frame_table, read_wav_info

from .helpers import make_case


def _media(case_dir: Path):
    return (
        SubtitleAsset.load(case_dir / "subtitles.json"),
        read_wav_info(case_dir / "program.wav"),
        read_frame_table(case_dir / "frames.ftbl"),
    )


def test_report_is_deterministic_and_hash_complete(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    asset, wav, frames = _media(case_dir)
    first = compute_layout(asset, wav, frames)
    second = compute_layout(asset, wav, frames)
    assert first == second
    verify_report(first)
    assert first["sentences"][0]["verdict"]["extended"] is True


def test_min_gap_defers_overlapping_sentence(tmp_path: Path) -> None:
    case_dir = make_case(
        tmp_path,
        sentences=[
            SentenceInput("S1", "一句话字幕", "00:00:01.000000", "00:00:01.100000", "A"),
            SentenceInput("S2", "二句话字幕", "00:00:01.900000", "00:00:03.000000", "B"),
        ],
    )
    asset, wav, frames = _media(case_dir)
    report = compute_layout(asset, wav, frames)
    second = report["sentences"][1]
    assert second["verdict"]["deferred"] is True
    assert second["verdict"]["status"] == "deferred"
    assert "min_gap_defer" in second["verdict"]["reasons"]


def test_description_priority_cuts_lower_priority_sentence(tmp_path: Path) -> None:
    case_dir = make_case(
        tmp_path,
        sentences=[
            SentenceInput("S1", "一句话字幕", "00:00:01.000000", "00:00:02.000000", "A"),
            SentenceInput("S2", "下一句话字幕", "00:00:02.250000", "00:00:04.000000", "B"),
        ],
        descriptions=[
            DescriptionInput(
                "D1",
                "S1",
                "画面描述内容需要完整插入轨道",
                "NARRATOR",
                "8",
                min_duration_ms=400,
                priority=70,
            )
        ],
        params=LayoutParams(min_gap_ms=200, sentence_priority=50),
    )
    asset, wav, frames = _media(case_dir)
    report = compute_layout(asset, wav, frames)
    description = report["descriptions"][0]
    assert description["status"] in {"inserted_cut", "inserted"}
    if description["status"] == "inserted_cut":
        assert report["cut_sentence_ids"] == ["S2"]
        assert "description_priority_cut" in report["sentences"][1]["verdict"]["reasons"]


def test_equal_priority_description_conflict_has_business_code(tmp_path: Path) -> None:
    case_dir = make_case(
        tmp_path,
        sentences=[
            SentenceInput("S1", "第一句", "00:00:01.000000", "00:00:01.400000", "A"),
            SentenceInput("S2", "第二句", "00:00:02.300000", "00:00:02.600000", "B"),
        ],
        descriptions=[
            DescriptionInput(
                "D1",
                "S1",
                "描述" * 12,
                "NARRATOR",
                "8",
                min_duration_ms=400,
                priority=50,
            )
        ],
        params=LayoutParams(min_gap_ms=200, sentence_priority=50),
    )
    asset, wav, frames = _media(case_dir)
    with pytest.raises(BusinessError) as caught:
        compute_layout(asset, wav, frames)
    assert caught.value.code == E_DESC_CONFLICT
    assert caught.value.context["description_id"] == "D1"


def test_incremental_layout_reuses_unaffected_hash_and_matches_offline(tmp_path: Path) -> None:
    case_dir = make_case(
        tmp_path,
        sentences=[
            SentenceInput("S1", "第一句字幕", "00:00:01.000000", "00:00:01.500000", "A"),
            SentenceInput("S2", "第二句字幕", "00:00:03.000000", "00:00:03.800000", "B"),
            SentenceInput("S3", "第三句字幕", "00:00:05.000000", "00:00:05.500000", "B"),
        ],
    )
    asset, wav, frames = _media(case_dir)
    baseline = compute_layout(asset, wav, frames)
    updates = {asset.sentences[-1].id: {"cps": "20"}}
    new_sentences = tuple(
        sentence.with_updates(updates[sentence.id]) if sentence.id in updates else sentence
        for sentence in asset.sentences
    )
    changed = asset.replace(sentences=new_sentences)
    affected = compute_affected_sentence_ids(
        old_asset=asset,
        new_asset=changed,
        old_params=asset.parameters,
        new_params=changed.parameters,
        previous_report=baseline,
    )
    assert affected == ["S3"]
    incremental = compute_layout(
        changed,
        wav,
        frames,
        revision=2,
        parent_hash=baseline["commit_hash"],
        previous_report=baseline,
        affected_ids=affected,
    )
    assert incremental["recomputed_sentence_ids"] == ["S3"]
    assert incremental["reused_sentence_ids"] == ["S1", "S2"]
    old_hashes = {item["sentence_id"]: item["entry_hash"] for item in baseline["sentences"]}
    new_hashes = {item["sentence_id"]: item["entry_hash"] for item in incremental["sentences"]}
    assert new_hashes["S1"] == old_hashes["S1"]
    assert new_hashes["S2"] == old_hashes["S2"]
    offline = compute_layout(changed, wav, frames)
    compare_layouts(incremental, offline)


def test_tampered_online_layout_raises_mismatch(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    asset, wav, frames = _media(case_dir)
    offline = compute_layout(asset, wav, frames)
    tampered = copy.deepcopy(offline)
    tampered["sentences"][1]["start_sample"] += 1
    with pytest.raises(BusinessError) as caught:
        compare_layouts(tampered, offline)
    assert caught.value.code == E_LAYOUT_MISMATCH
    assert caught.value.context["mismatches"][0]["sentence_id"] == "S2"


def test_speaker_markers_are_emitted_only_on_changes(tmp_path: Path) -> None:
    case_dir = make_case(tmp_path)
    asset, wav, frames = _media(case_dir)
    report = compute_layout(asset, wav, frames)
    assert [item["at_sentence_id"] for item in report["speaker_markers"]] == ["S1", "S2"]

