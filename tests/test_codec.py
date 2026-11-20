from __future__ import annotations

import binascii
import struct

import numpy as np
import pytest

from atlas_pipeline.codec import (
    HEADER_STRUCT,
    decode_atlas_bytes,
    encode_atlas_bytes,
    read_atlas,
)
from atlas_pipeline.errors import AtlasError, ErrorCode
from atlas_pipeline.sample import generate_atlas


def _rewrite_header(data: bytes, **changes: int) -> bytes:
    fields = list(HEADER_STRUCT.unpack_from(data, 0))
    indexes = {
        "format_version": 1,
        "header_size": 2,
        "width": 3,
        "height": 4,
        "bit_depth": 5,
        "color_code": 6,
        "transfer_code": 7,
        "storage_code": 8,
        "grid_cols": 9,
        "grid_rows": 10,
        "payload_size": 13,
    }
    for key, value in changes.items():
        fields[indexes[key]] = value
    fields[15] = 0
    crc = binascii.crc32(HEADER_STRUCT.pack(*fields)) & 0xFFFFFFFF
    fields[15] = crc
    return HEADER_STRUCT.pack(*fields) + data[HEADER_STRUCT.size :]


def test_atlas_roundtrip_exposes_grid_and_linear_channels(tmp_path):
    path = tmp_path / "roundtrip.atlas"
    atlas = generate_atlas(
        path,
        width=32,
        height=24,
        grid_cols=2,
        grid_rows=2,
        transfer="pq",
        source_peak_nits=1000.0,
    )
    reloaded = read_atlas(path)
    assert reloaded.header.width == 32
    assert reloaded.header.height == 24
    assert reloaded.header.transfer_function == "pq"
    assert reloaded.tile_count == 4
    assert np.array_equal(reloaded.encoded, atlas.encoded)
    assert np.all(np.isfinite(reloaded.linear))
    assert [tile.bbox for tile in reloaded.tiles] == [
        (0, 0, 16, 12),
        (16, 0, 32, 12),
        (0, 12, 16, 24),
        (16, 12, 32, 24),
    ]


def test_unknown_transfer_function_reports_exact_field(tmp_path):
    atlas = generate_atlas(tmp_path / "a.atlas", width=16, height=16, grid_cols=2, grid_rows=2)
    data = _rewrite_header(atlas.raw_bytes, transfer_code=99)
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(data)
    assert caught.value.code == ErrorCode.E_ATLAS_INVALID
    assert caught.value.field == "transfer_function_code"


def test_nondivisible_grid_reports_axis_field(tmp_path):
    atlas = generate_atlas(tmp_path / "a.atlas", width=32, height=24, grid_cols=2, grid_rows=2)
    data = _rewrite_header(atlas.raw_bytes, grid_cols=3)
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(data)
    assert caught.value.code == ErrorCode.E_ATLAS_INVALID
    assert caught.value.field == "grid_cols"


def test_header_crc_mismatch_is_diagnosable(tmp_path):
    atlas = generate_atlas(tmp_path / "a.atlas", width=16, height=16, grid_cols=2, grid_rows=2)
    data = bytearray(atlas.raw_bytes)
    data[20] ^= 0x01
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(bytes(data))
    assert caught.value.code == ErrorCode.E_ATLAS_INVALID
    assert caught.value.field == "header_crc32"


def test_payload_length_mismatch_is_diagnosable(tmp_path):
    atlas = generate_atlas(tmp_path / "a.atlas", width=16, height=16, grid_cols=2, grid_rows=2)
    data = atlas.raw_bytes[:-2]
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(data)
    assert caught.value.code == ErrorCode.E_ATLAS_INVALID
    assert caught.value.field == "payload_size"


def test_payload_crc_mismatch_is_diagnosable(tmp_path):
    atlas = generate_atlas(tmp_path / "a.atlas", width=16, height=16, grid_cols=2, grid_rows=2)
    data = bytearray(atlas.raw_bytes)
    data[-1] ^= 0x01
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(bytes(data))
    assert caught.value.field == "payload_crc32"


def test_contradictory_bit_depth_and_storage_are_rejected(tmp_path):
    atlas = generate_atlas(tmp_path / "a.atlas", width=16, height=16, grid_cols=2, grid_rows=2)
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(_rewrite_header(atlas.raw_bytes, bit_depth=11))
    assert caught.value.field == "bit_depth"
    with pytest.raises(AtlasError) as caught:
        decode_atlas_bytes(_rewrite_header(atlas.raw_bytes, storage_code=2))
    assert caught.value.field == "storage_format"


def test_writer_validates_shape_and_grid():
    pixels = np.zeros((8, 8, 4), dtype=np.float16)
    encoded = encode_atlas_bytes(
        pixels,
        bit_depth=10,
        color_space="rec709",
        transfer="linear",
        grid_cols=2,
        grid_rows=2,
        source_peak_nits=1000.0,
        pipeline_version=9,
    )
    decoded = decode_atlas_bytes(encoded)
    assert decoded.header.pipeline_version == 9
    with pytest.raises(AtlasError) as caught:
        encode_atlas_bytes(
            pixels,
            bit_depth=10,
            color_space="rec709",
            transfer="linear",
            grid_cols=3,
            grid_rows=2,
            source_peak_nits=1000.0,
        )
    assert caught.value.field == "grid_cols"
