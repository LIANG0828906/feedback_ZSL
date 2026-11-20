from __future__ import annotations

from pathlib import Path

import pytest

from streamforge.errors import BusinessError, ErrorCode
from streamforge.timebase import build_timeline, read_pcm16_wav
from tests.conftest import read_json, write_json


def test_reads_supported_pcm_variants(sample_bundle: dict[str, Path]) -> None:
    mono = read_pcm16_wav(sample_bundle["audio_mono"])
    stereo = read_pcm16_wav(sample_bundle["audio_44k"])
    assert mono.sample_rate == 16000
    assert mono.channels == 1
    assert stereo.sample_rate == 44100
    assert stereo.channels == 2


def test_builds_shared_contiguous_boundaries(sample_bundle: dict[str, Path]) -> None:
    timeline = build_timeline(
        audio_path=sample_bundle["audio_mono"],
        video_path=sample_bundle["video"],
        slice_duration_ms=400,
        tolerance_ms=2,
    )
    assert len(timeline.boundaries) == 3
    assert timeline.boundaries[0].source_start_sample == 0
    assert timeline.boundaries[-1].source_end_sample == 19200
    for previous, current in zip(timeline.boundaries, timeline.boundaries[1:]):
        assert previous.end_ns == current.start_ns
        assert previous.source_end_sample == current.source_start_sample
        assert current.start_ns in timeline.video_frames_by_pts


@pytest.mark.parametrize(
    ("mutation", "error_detail"),
    [
        (lambda doc: doc["frames"][2].update({"pts_ms": doc["frames"][1]["pts_ms"]}), "frame_index"),
        (lambda doc: doc["frames"][2].update({"pts_ms": 30}), "frame_index"),
        (lambda doc: doc["frames"][1].update({"duration_ms": 0}), "frame_index"),
        (lambda doc: doc["frames"][2].update({"pts_ms": 90}), "frame_index"),
    ],
)
def test_rejects_bad_video_timelines(
    sample_bundle: dict[str, Path],
    tmp_path: Path,
    mutation,
    error_detail: str,
) -> None:
    document = read_json(sample_bundle["video"])
    mutation(document)
    bad_video = tmp_path / "bad_video.json"
    write_json(bad_video, document)
    with pytest.raises(BusinessError) as caught:
        build_timeline(
            audio_path=sample_bundle["audio"],
            video_path=bad_video,
            slice_duration_ms=400,
            tolerance_ms=2,
        )
    assert caught.value.code == ErrorCode.TIMEBASE_INVALID
    assert error_detail in caught.value.details


def test_rejects_audio_video_duration_mismatch(
    sample_bundle: dict[str, Path], tmp_path: Path
) -> None:
    document = read_json(sample_bundle["video"])
    for index in range(30, 40):
        document["frames"].append(
            {
                "display_index": index,
                "duration_ms": 40,
                "pts_ms": index * 40,
            }
        )
    document["duration_ns"] = 1600 * 1_000_000
    bad_video = tmp_path / "long_video.json"
    write_json(bad_video, document)
    with pytest.raises(BusinessError) as caught:
        build_timeline(
            audio_path=sample_bundle["audio"],
            video_path=bad_video,
            slice_duration_ms=400,
            tolerance_ms=2,
        )
    assert caught.value.code == ErrorCode.TIMEBASE_INVALID
    assert caught.value.details["audio_sample_position"] == 57600
    assert caught.value.details["frame_index"] == 39


def test_rejects_boundary_not_on_frame_pts(sample_bundle: dict[str, Path]) -> None:
    with pytest.raises(BusinessError) as caught:
        build_timeline(
            audio_path=sample_bundle["audio"],
            video_path=sample_bundle["video"],
            slice_duration_ms=450,
            tolerance_ms=2,
        )
    assert caught.value.code == ErrorCode.TIMEBASE_INVALID
    assert caught.value.details["boundary_index"] == 1
    assert caught.value.message.startswith("slice boundary does not align")


def test_rejects_negative_display_index(
    sample_bundle: dict[str, Path], tmp_path: Path
) -> None:
    document = read_json(sample_bundle["video"])
    document["frames"][1]["display_index"] = -1
    bad_video = tmp_path / "negative_display.json"
    write_json(bad_video, document)
    with pytest.raises(BusinessError) as caught:
        build_timeline(
            audio_path=sample_bundle["audio"],
            video_path=bad_video,
            slice_duration_ms=400,
            tolerance_ms=2,
        )
    assert caught.value.code == ErrorCode.TIMEBASE_INVALID
    assert caught.value.details["frame_index"] == 1
