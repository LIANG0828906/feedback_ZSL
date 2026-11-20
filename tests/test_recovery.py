from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from hdr_studio.errors import ErrorCode, HDRError
from hdr_studio.model import (
    ColorSpace,
    ConversionParams,
    ExposureSegment,
    ToneMap,
    TransferFunction,
)
from hdr_studio.session import ConversionSession
from hdr_studio.util import canonical_bytes, sha256_hex


def test_snapshot_restores_versions_and_preview(
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
            exposure_segments=(ExposureSegment(1, 3, -0.5),),
        )
    )
    preview = published_session.set_preview(
        selected_version=v1.version, frame_index=2, playing=False
    )
    workspace = published_session.workspace
    published_session.close()

    restored = ConversionSession(workspace)
    assert [record.version for record in restored.versions] == [1, 2]
    assert restored.latest_record == v2
    assert restored.preview_state == preview
    assert restored.recovery_report is not None
    assert restored.recovery_report.restored_version == 2
    assert restored.recovery_report.recomputed_frames == {}


def test_recovery_recomputes_only_missing_frame(
    published_session: ConversionSession,
) -> None:
    workspace = published_session.workspace
    record = published_session.latest_record
    assert record is not None
    frame_path = (
        published_session.versions_dir
        / "v000001"
        / "frames"
        / "frame_000003.bin"
    )
    expected_hash = record.frame_hashes[3]
    frame_path.unlink()
    published_session.close()

    restored = ConversionSession(workspace)
    assert restored.recovery_report is not None
    assert restored.recovery_report.recomputed_frames == {1: (3,)}
    assert frame_path.exists()
    from hdr_studio.util import hash_file

    assert hash_file(frame_path) == expected_hash


def test_corrupt_published_frame_aborts_recovery(
    published_session: ConversionSession,
) -> None:
    workspace = published_session.workspace
    frame_path = (
        published_session.versions_dir
        / "v000001"
        / "frames"
        / "frame_000000.bin"
    )
    data = bytearray(frame_path.read_bytes())
    data[0] ^= 0x01
    frame_path.write_bytes(data)
    published_session.close()
    with pytest.raises(HDRError) as caught:
        ConversionSession(workspace)
    assert caught.value.code is ErrorCode.RECOVERY_INVALID
    assert "hash mismatch" in caught.value.message


def test_missing_snapshot_field_aborts_recovery(
    published_session: ConversionSession,
) -> None:
    workspace = published_session.workspace
    snapshot = json.loads(published_session.snapshot_path.read_text("utf-8"))
    del snapshot["versions"][0]["operation"]
    published_session.snapshot_path.write_text(json.dumps(snapshot), "utf-8")
    published_session.close()
    with pytest.raises(HDRError) as caught:
        ConversionSession(workspace)
    assert caught.value.code is ErrorCode.RECOVERY_INVALID


def test_broken_version_hash_chain_aborts_recovery(
    published_session: ConversionSession,
) -> None:
    published_session.submit(
        ConversionParams(
            target_color_space=ColorSpace.REC709,
            target_transfer=TransferFunction.SDR_GAMMA,
            peak_luminance=100.0,
            tone_map=ToneMap.REINHARD,
            exposure_segments=(ExposureSegment(1, 2, -0.25),),
        )
    )
    workspace = published_session.workspace
    snapshot = json.loads(published_session.snapshot_path.read_text("utf-8"))
    snapshot["versions"][1]["parent_record_hash"] = "0" * 64
    snapshot.pop("snapshot_hash")
    snapshot["snapshot_hash"] = sha256_hex(canonical_bytes(snapshot))
    published_session.snapshot_path.write_bytes(canonical_bytes(snapshot))
    published_session.close()
    with pytest.raises(HDRError) as caught:
        ConversionSession(workspace)
    assert caught.value.code is ErrorCode.RECOVERY_INVALID
    assert "hash chain" in caught.value.message


def test_forced_termination_restores_last_published_snapshot(
    sample_workspace: tuple[Path, Path],
) -> None:
    workspace, sample = sample_workspace
    session = ConversionSession(workspace, sample)
    v1 = session.submit(
        ConversionParams(
            target_color_space=ColorSpace.REC709,
            target_transfer=TransferFunction.SDR_GAMMA,
            peak_luminance=100.0,
            tone_map=ToneMap.REINHARD,
        )
    )
    session.close()
    script = f"""
import os
from hdr_studio.model import ColorSpace, ConversionParams, ToneMap, TransferFunction
from hdr_studio.session import ConversionSession
s = ConversionSession({str(workspace)!r}, {str(sample)!r})
def crash():
    os._exit(97)
s._after_version_dir_visible = crash
s.submit(ConversionParams(ColorSpace.DISPLAY_P3, TransferFunction.HLG, 600.0, ToneMap.HABLE))
"""
    env = dict(os.environ)
    env["PYTHONPATH"] = str(Path.cwd())
    result = subprocess.run(
        [sys.executable, "-c", script],
        cwd=workspace.parent,
        check=False,
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 97, result.stderr
    assert (workspace / "versions" / "v000002").exists()
    assert json.loads((workspace / "snapshots" / "current.json").read_text("utf-8"))[
        "latest_version"
    ] == 1

    restored = ConversionSession(workspace)
    assert restored.latest_record == v1
    assert [record.version for record in restored.versions] == [1]
    assert restored.recovery_report is not None
    assert restored.recovery_report.discarded_versions == (2,)
    assert not (workspace / "versions" / "v000002").exists()

