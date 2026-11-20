from __future__ import annotations

import threading
import time
from pathlib import Path

import pytest

import hdr_studio.session as session_module
from hdr_studio.errors import ErrorCode, HDRError
from hdr_studio.model import (
    ColorSpace,
    ConversionParams,
    ExposureSegment,
    ToneMap,
    TransferFunction,
)
from hdr_studio.session import ConversionSession


def test_incremental_version_reuses_unaffected_frames(
    published_session: ConversionSession, valid_params: ConversionParams
) -> None:
    v1 = published_session.latest_record
    assert v1 is not None
    params = ConversionParams(
        target_color_space=valid_params.target_color_space,
        target_transfer=valid_params.target_transfer,
        peak_luminance=valid_params.peak_luminance,
        tone_map=valid_params.tone_map,
        exposure_segments=(ExposureSegment(2, 4, -0.5),),
    )
    v2 = published_session.submit(params)
    assert v2.version == v1.version + 1
    assert v2.affected_intervals == ((2, 4),)
    assert v2.recomputed_count == 3
    assert v2.reused_count == published_session.header.frame_count - 3
    assert v2.frame_hashes[:2] == v1.frame_hashes[:2]
    assert v2.frame_hashes[5:] == v1.frame_hashes[5:]
    assert v2.parent_record_hash == v1.record_hash
    assert v2.verify_hash()


def test_diff_and_rollback_create_new_version(
    published_session: ConversionSession,
) -> None:
    v1 = published_session.latest_record
    assert v1 is not None
    v2 = published_session.submit(
        ConversionParams(
            target_color_space=ColorSpace.REC709,
            target_transfer=TransferFunction.SDR_GAMMA,
            peak_luminance=100.0,
            tone_map=ToneMap.REINHARD,
            exposure_segments=(ExposureSegment(0, 3, -0.25),),
        )
    )
    diff = published_session.diff_versions(v1.version, v2.version)
    assert diff.changed_params == ("exposure_segments",)
    assert diff.changed_frames
    assert not diff.same_content

    rolled = published_session.rollback(v1.version)
    assert rolled.version == v2.version + 1
    assert rolled.operation == "rollback"
    assert rolled.rollback_target == v1.version
    assert rolled.output_hash == v1.output_hash
    assert rolled.parent_record_hash == v2.record_hash
    assert rolled.recomputed_count == 0
    assert rolled.reused_count == published_session.header.frame_count
    assert [record.version for record in published_session.versions] == [
        v1.version,
        v2.version,
        rolled.version,
    ]


def test_illegal_params_do_not_change_any_published_artifact(
    published_session: ConversionSession,
) -> None:
    snapshot_before = published_session.snapshot_path.read_bytes()
    latest_before = published_session.latest_record
    assert latest_before is not None
    invalid = ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.NONE,
    )
    with pytest.raises(HDRError) as caught:
        published_session.submit(invalid)
    assert caught.value.code is ErrorCode.PARAM_CONFLICT
    assert set(caught.value.conflict_params) == {"peak_luminance", "tone_map"}
    assert published_session.snapshot_path.read_bytes() == snapshot_before
    assert published_session.latest_record == latest_before
    assert len(published_session.versions) == 1
    assert not list(published_session.versions_dir.glob("v000002"))


def test_exposure_overrange_is_rejected_without_staging_output(
    published_session: ConversionSession,
) -> None:
    invalid = ConversionParams(
        target_color_space=ColorSpace.REC709,
        target_transfer=TransferFunction.SDR_GAMMA,
        peak_luminance=100.0,
        tone_map=ToneMap.REINHARD,
        exposure_segments=(ExposureSegment(1, 3, 4.0),),
    )
    with pytest.raises(HDRError) as caught:
        published_session.submit(invalid)
    assert caught.value.code is ErrorCode.PARAM_CONFLICT
    assert caught.value.conflict_params == ["exposure_segments[0]"]
    assert caught.value.details["max_raw_code"] > caught.value.details["max_code"]
    assert not list(published_session.versions_dir.glob(".staging-*"))
    assert len(published_session.versions) == 1


def test_extreme_parameter_values_are_rejected(
    published_session: ConversionSession,
) -> None:
    with pytest.raises(HDRError) as peak_error:
        published_session.submit(
            ConversionParams(
                target_color_space=ColorSpace.REC709,
                target_transfer=TransferFunction.SDR_GAMMA,
                peak_luminance=10001.0,
                tone_map=ToneMap.REINHARD,
            )
        )
    assert peak_error.value.code is ErrorCode.PARAM_CONFLICT
    assert peak_error.value.conflict_params == ["peak_luminance"]

    with pytest.raises(HDRError) as ev_error:
        published_session.submit(
            ConversionParams(
                target_color_space=ColorSpace.REC709,
                target_transfer=TransferFunction.SDR_GAMMA,
                peak_luminance=100.0,
                tone_map=ToneMap.REINHARD,
                exposure_segments=(ExposureSegment(0, 0, 65.0),),
            )
        )
    assert ev_error.value.code is ErrorCode.PARAM_CONFLICT
    assert ev_error.value.conflict_params == ["exposure_segments[0]"]
    assert published_session.latest_record.version == 1


def test_busy_query_and_cancel(
    published_session: ConversionSession,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    entered = threading.Event()
    release = threading.Event()
    original = session_module.convert_frame

    def slow_convert(*args: object, **kwargs: object):
        entered.set()
        release.wait(10)
        return original(*args, **kwargs)  # type: ignore[arg-type]

    monkeypatch.setattr(session_module, "convert_frame", slow_convert)
    params = ConversionParams(
        target_color_space=ColorSpace.REC2020,
        target_transfer=TransferFunction.PQ,
        peak_luminance=1000.0,
        tone_map=ToneMap.HABLE,
    )
    errors: list[HDRError] = []

    def run() -> None:
        try:
            published_session.submit(params)
        except HDRError as exc:
            errors.append(exc)

    worker = threading.Thread(target=run)
    worker.start()
    assert entered.wait(5)
    with pytest.raises(HDRError) as busy:
        published_session.submit(params)
    assert busy.value.code is ErrorCode.BUSY
    assert published_session.latest_record.version == 1
    assert published_session.cancel_active() is True
    release.set()
    worker.join(10)
    assert errors and errors[0].code is ErrorCode.CANCELLED
    assert published_session.latest_record.version == 1
    assert not list(published_session.versions_dir.glob(".staging-*"))
    assert not published_session.is_busy


def test_query_never_observes_a_partial_version(
    published_session: ConversionSession,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    entered = threading.Event()
    release = threading.Event()
    original = session_module.convert_frame

    def slow_convert(*args: object, **kwargs: object):
        entered.set()
        release.wait(10)
        return original(*args, **kwargs)  # type: ignore[arg-type]

    monkeypatch.setattr(session_module, "convert_frame", slow_convert)
    params = ConversionParams(
        target_color_space=ColorSpace.DISPLAY_P3,
        target_transfer=TransferFunction.HLG,
        peak_luminance=600.0,
        tone_map=ToneMap.HABLE,
    )
    observed: list[int] = []
    worker = threading.Thread(target=lambda: published_session.submit(params))
    worker.start()
    assert entered.wait(5)
    for _ in range(30):
        observed.append(published_session.latest_record.version)
        time.sleep(0.002)
    release.set()
    worker.join(10)
    assert set(observed) == {1}
    assert published_session.latest_record.version == 2


def test_version_mismatch_aborts_publication(
    published_session: ConversionSession,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    latest = published_session.latest_record
    assert latest is not None

    def corrupt_stage(staging: Path, record: object) -> None:
        path = staging / "frames" / "frame_000000.bin"
        data = bytearray(path.read_bytes())
        data[0] ^= 0xFF
        path.write_bytes(data)

    monkeypatch.setattr(
        published_session, "_before_publish_commit", corrupt_stage
    )
    with pytest.raises(HDRError) as caught:
        published_session.submit(
            ConversionParams(
                target_color_space=ColorSpace.REC709,
                target_transfer=TransferFunction.SDR_GAMMA,
                peak_luminance=100.0,
                tone_map=ToneMap.HABLE,
            )
        )
    assert caught.value.code is ErrorCode.VERSION_MISMATCH
    assert published_session.latest_record == latest
    assert len(published_session.versions) == 1
    assert not list(published_session.versions_dir.glob(".staging-*"))
    assert not (published_session.versions_dir / "v000002").exists()

