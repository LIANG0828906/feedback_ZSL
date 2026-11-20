from __future__ import annotations

import struct
import zlib
from pathlib import Path

import pytest

from hdr_studio.errors import ErrorCode, HDRError
from hdr_studio.formats import (
    HEADER_BODY_STRUCT,
    HEADER_SIZE,
    HDRSequence,
    build_header,
    expected_frame_bytes,
)
from hdr_studio.generator import generate_sample_set
from hdr_studio.model import ColorSpace, TransferFunction


def test_header_and_frames_are_parsed(tmp_path: Path) -> None:
    manifest = generate_sample_set(tmp_path)
    entry = manifest["samples"][0]
    sequence = HDRSequence(tmp_path / "samples" / entry["file"])
    assert sequence.header.width == entry["width"]
    assert sequence.header.height == entry["height"]
    assert sequence.header.frame_count == entry["frame_count"]
    assert sequence.header.bit_depth == entry["bit_depth"]
    assert sequence.header.color_space is ColorSpace(entry["color_space"])
    assert sequence.header.transfer is TransferFunction(entry["transfer"])
    assert sequence.header.peak_luminance == entry["peak_luminance"]
    frame = sequence.read_frame(0)
    assert frame.shape == (entry["height"], entry["width"], 3)
    assert int(frame.max()) <= (1 << entry["bit_depth"]) - 1


def _valid_bytes(tmp_path: Path) -> bytes:
    path = tmp_path / "valid.hdrseq"
    header = build_header(
        width=2,
        height=2,
        frame_count=2,
        bit_depth=10,
        color_space=ColorSpace.REC2020,
        transfer=TransferFunction.PQ,
        peak_luminance=1000.0,
    )
    path.write_bytes(header + b"\x00" * (2 * expected_frame_bytes(2, 2, 10)))
    return path.read_bytes()


def test_incomplete_header_is_rejected(tmp_path: Path) -> None:
    path = tmp_path / "short.hdrseq"
    path.write_bytes(b"HDRSEQ01")
    with pytest.raises(HDRError) as caught:
        HDRSequence(path)
    assert caught.value.code is ErrorCode.HDR_INVALID


def test_bit_depth_data_length_mismatch_is_rejected(tmp_path: Path) -> None:
    data = bytearray(_valid_bytes(tmp_path))
    fields = list(HEADER_BODY_STRUCT.unpack(data[: HEADER_SIZE - 4]))
    fields[6] = 8
    body = HEADER_BODY_STRUCT.pack(*fields)
    data[: HEADER_SIZE - 4] = body
    data[HEADER_SIZE - 4 : HEADER_SIZE] = struct.pack(
        "<I", zlib.crc32(body) & 0xFFFFFFFF
    )
    path = tmp_path / "bad-depth.hdrseq"
    path.write_bytes(data)
    with pytest.raises(HDRError) as caught:
        HDRSequence(path)
    assert caught.value.code is ErrorCode.HDR_INVALID
    assert "frame data length" in caught.value.message


def test_unknown_transfer_code_is_rejected(tmp_path: Path) -> None:
    data = bytearray(_valid_bytes(tmp_path))
    fields = list(HEADER_BODY_STRUCT.unpack(data[: HEADER_SIZE - 4]))
    fields[8] = 250
    body = HEADER_BODY_STRUCT.pack(*fields)
    data[: HEADER_SIZE - 4] = body
    data[HEADER_SIZE - 4 : HEADER_SIZE] = struct.pack(
        "<I", zlib.crc32(body) & 0xFFFFFFFF
    )
    path = tmp_path / "bad-transfer.hdrseq"
    path.write_bytes(data)
    with pytest.raises(HDRError) as caught:
        HDRSequence(path)
    assert caught.value.code is ErrorCode.HDR_INVALID
    assert caught.value.details["transfer_code"] == 250


def test_sample_exceeding_declared_depth_is_rejected(tmp_path: Path) -> None:
    data = bytearray(_valid_bytes(tmp_path))
    data[HEADER_SIZE : HEADER_SIZE + 2] = struct.pack("<H", 0xFFFF)
    path = tmp_path / "bad-sample.hdrseq"
    path.write_bytes(data)
    sequence = HDRSequence(path)
    with pytest.raises(HDRError) as caught:
        sequence.read_frame(0)
    assert caught.value.code is ErrorCode.HDR_INVALID


def test_truncated_frame_sequence_is_rejected(tmp_path: Path) -> None:
    data = _valid_bytes(tmp_path)
    path = tmp_path / "truncated.hdrseq"
    path.write_bytes(data[:-1])
    with pytest.raises(HDRError) as caught:
        HDRSequence(path)
    assert caught.value.code is ErrorCode.HDR_INVALID

