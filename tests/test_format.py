import shutil
import struct

import numpy as np
import pytest

from sciworkbench.errors import E_IMG_INVALID, WorkbenchError
from sciworkbench.model import (
    HEADER_STRUCT,
    PixelFormat,
    load_sequence,
    write_sequence,
)


def _mutate_header(source, destination, field_index, value):
    raw = bytearray(source.read_bytes())
    fields = list(HEADER_STRUCT.unpack(raw[: HEADER_STRUCT.size]))
    fields[field_index] = value
    HEADER_STRUCT.pack_into(raw, 0, *fields)
    destination.write_bytes(raw)


def test_roundtrip_structure_and_block_coverage(tmp_path):
    frames = np.arange(2 * 10 * 12, dtype=np.uint16).reshape(2, 10, 12)
    path = tmp_path / "roundtrip.simg"
    write_sequence(path, frames)
    sequence = load_sequence(path, block_size=4)

    assert np.array_equal(sequence.pixels, frames)
    assert sequence.header.width == 12
    assert sequence.header.height == 10
    assert sequence.header.frame_count == 2
    assert sequence.header.bit_depth == 16
    assert sequence.structure.block_rows == 3
    assert sequence.structure.block_cols == 3
    assert sequence.structure.total_blocks == 18

    coverage = np.zeros((10, 12), dtype=np.int64)
    for block in sequence.structure.iter_frame_blocks(0):
        coverage[block.y0 : block.y1, block.x0 : block.x1] += 1
    assert np.all(coverage == 1)


def test_big_endian_roundtrip(tmp_path):
    frames = np.array([[[1, 256], [65535, 300]]], dtype=np.uint16)
    path = tmp_path / "be.simg"
    write_sequence(path, frames, pixel_format=PixelFormat.MONO16_BE)
    sequence = load_sequence(path)
    assert np.array_equal(sequence.pixels, frames)


def test_invalid_magic_reports_frame_zero(tmp_path):
    path = tmp_path / "ok.simg"
    write_sequence(path, np.zeros((2, 8, 8), dtype=np.uint16))
    bad = tmp_path / "bad.simg"
    _mutate_header(path, bad, 0, b"BADMAGIC")
    with pytest.raises(WorkbenchError) as caught:
        load_sequence(bad)
    assert caught.value.code == E_IMG_INVALID
    assert caught.value.frame == 0


def test_unknown_pixel_format_is_rejected(tmp_path):
    path = tmp_path / "ok.simg"
    write_sequence(path, np.zeros((2, 8, 8), dtype=np.uint16))
    bad = tmp_path / "unknown.simg"
    _mutate_header(path, bad, 7, 999)
    with pytest.raises(WorkbenchError) as caught:
        load_sequence(bad)
    assert caught.value.code == E_IMG_INVALID
    assert "pixel format" in caught.value.message


def test_header_stride_contradiction_is_rejected(tmp_path):
    path = tmp_path / "ok.simg"
    write_sequence(path, np.zeros((2, 8, 8), dtype=np.uint16))
    bad = tmp_path / "stride.simg"
    _mutate_header(path, bad, 9, 1)
    with pytest.raises(WorkbenchError) as caught:
        load_sequence(bad)
    assert caught.value.code == E_IMG_INVALID
    assert caught.value.frame == 0


def test_truncation_reports_first_incomplete_frame(tmp_path):
    path = tmp_path / "ok.simg"
    write_sequence(path, np.zeros((5, 8, 8), dtype=np.uint16))
    truncated = tmp_path / "truncated.simg"
    shutil.copyfile(path, truncated)
    size = truncated.stat().st_size
    frame_bytes = 8 * 8 * 2
    with truncated.open("r+b") as handle:
        handle.truncate(size - frame_bytes - 17)
    with pytest.raises(WorkbenchError) as caught:
        load_sequence(truncated)
    assert caught.value.code == E_IMG_INVALID
    assert caught.value.frame == 3


def test_extra_trailing_bytes_are_rejected(tmp_path):
    path = tmp_path / "ok.simg"
    write_sequence(path, np.zeros((2, 8, 8), dtype=np.uint16))
    with path.open("ab") as handle:
        handle.write(b"x")
    with pytest.raises(WorkbenchError) as caught:
        load_sequence(path)
    assert caught.value.code == E_IMG_INVALID
    assert caught.value.frame == 1
