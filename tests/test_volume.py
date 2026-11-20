from __future__ import annotations

import math
from pathlib import Path
import struct

import numpy as np
import pytest

from volrender.errors import VolumeInvalidError
from volrender.injection import inject_volume_bytes, load_with_injection
from volrender.volume import (
    MASK_OFFSET_OFFSET,
    SCALAR_REFERENCE_OFFSET,
    SPACING_OFFSET,
    WINDOW_WIDTH_OFFSET,
    build_header,
    load_volume,
    parse_header,
)


def test_load_normalizes_scalar_and_mask(volume_path: Path) -> None:
    volume = load_volume(volume_path)
    assert volume.dims == (16, 16, 12)
    assert volume.scalar.shape == volume.dims
    assert volume.mask.shape == volume.dims
    assert volume.scalar.dtype == np.float32
    assert volume.mask.dtype == np.float32
    assert 0.0 <= float(volume.scalar.min()) <= float(volume.scalar.max()) <= 1.0
    assert 0.0 <= float(volume.mask.min()) <= float(volume.mask.max()) <= 1.0
    assert volume.normalization["scalar"]["window_low"] == 3000.0
    assert volume.normalization["scalar"]["window_high"] == 53000.0
    assert volume.normalization["scalar"]["source"] == "header.window_width/window_level"
    assert volume.normalization["mask"]["max_label"] == 2.0
    assert volume.normalization["mask"]["source"] == "data.max(mask)"
    assert volume.volume_sha256 == __import__("hashlib").sha256(volume_path.read_bytes()).hexdigest()


def test_window_override_records_truth_source(volume_path: Path) -> None:
    volume = load_volume(volume_path, window_override=(10000.0, 20000.0))
    assert volume.normalization["scalar"]["window_low"] == 15000.0
    assert volume.normalization["scalar"]["window_high"] == 25000.0
    assert volume.normalization["scalar"]["source"] == "render.window_override"


def test_header_round_trip(volume_path: Path) -> None:
    header = parse_header(volume_path.read_bytes())
    rebuilt = build_header(
        dims=header.dims,
        spacing=header.spacing,
        window_width=header.window_width,
        window_level=header.window_level,
        mask_offset=header.mask_offset,
    )
    assert rebuilt == volume_path.read_bytes()[:128]


@pytest.mark.parametrize(
    ("mutation", "detail", "field", "offset"),
    [
        (lambda b: struct.pack_into("<B", b, 20, 99), "UNKNOWN_SCALAR_TYPE", "scalar_type", 20),
        (lambda b: struct.pack_into("<d", b, WINDOW_WIDTH_OFFSET, math.inf), "WINDOW_NON_FINITE", "window_width", 48),
        (lambda b: struct.pack_into("<d", b, SPACING_OFFSET, 0.0), "BAD_SPACING", "spacing[0]", 24),
        (lambda b: struct.pack_into("<d", b, SCALAR_REFERENCE_OFFSET, math.nan), "NON_FINITE_REFERENCE", "scalar_reference_max", 88),
        (lambda b: struct.pack_into("<Q", b, MASK_OFFSET_OFFSET, 1), "MASK_OFFSET_CONFLICT", "mask_offset", 64),
    ],
)
def test_header_errors_are_located(
    volume_path: Path,
    mutation,
    detail: str,
    field: str,
    offset: int,
) -> None:
    data = bytearray(volume_path.read_bytes())
    mutation(data)
    with pytest.raises(VolumeInvalidError) as raised:
        load_volume(bytes(data))
    assert raised.value.code == "E_VOLUME_INVALID"
    assert raised.value.detail == detail
    assert raised.value.field == field
    assert raised.value.offset == offset


def test_truncated_payload_is_rejected_before_render(volume_path: Path) -> None:
    original = volume_path.read_bytes()
    header = parse_header(original)
    data = original[: header.header_size + header.scalar_nbytes // 2]
    with pytest.raises(VolumeInvalidError) as raised:
        load_volume(data)
    assert raised.value.detail == "DATA_TRUNCATED"
    assert raised.value.field == "voxel_data"
    assert raised.value.offset == len(data)


def test_truncated_mask_is_distinguished(volume_path: Path) -> None:
    data = volume_path.read_bytes()[:-17]
    with pytest.raises(VolumeInvalidError) as raised:
        load_volume(data)
    assert raised.value.detail == "DATA_TRUNCATED"
    assert raised.value.field == "mask_data"
    assert raised.value.offset == len(data)


def test_dimension_length_mismatch_is_located(volume_path: Path) -> None:
    data = bytearray(volume_path.read_bytes())
    header = parse_header(data)
    struct.pack_into("<I", data, 8, header.dims[0] - 1)
    with pytest.raises(VolumeInvalidError) as raised:
        load_volume(bytes(data))
    assert raised.value.detail == "DIM_LENGTH_MISMATCH"
    assert raised.value.field == "data_length"
    assert raised.value.offset == parse_header(data).expected_file_size


def test_injected_volume_errors_keep_injection_metadata(volume_path: Path) -> None:
    for injection in ("dim_mismatch", "window_nonfinite", "truncated_voxels"):
        with pytest.raises(VolumeInvalidError) as raised:
            load_with_injection(volume_path, injection)
        assert raised.value.inject == injection
        assert raised.value.injection_point == f"load_entry:{injection}"


def test_injection_does_not_mutate_source(volume_path: Path) -> None:
    before = volume_path.read_bytes()
    inject_volume_bytes(before, "window_nonfinite")
    assert volume_path.read_bytes() == before


def test_non_finite_float_voxel_is_rejected() -> None:
    dims = (2, 2, 2)
    header = build_header(
        dims=dims,
        spacing=(1.0, 1.0, 1.0),
        window_width=100.0,
        window_level=50.0,
        scalar_type_code=6,
        mask_type_code=1,
    )
    scalar = np.zeros(8, dtype="<f4")
    scalar[3] = np.nan
    payload = header + scalar.tobytes() + np.zeros(8, dtype="<u1").tobytes()
    with pytest.raises(VolumeInvalidError) as raised:
        load_volume(payload)
    assert raised.value.detail == "NON_FINITE_VOXEL"
    assert raised.value.field == "voxel_data"
    assert raised.value.offset == 128 + 3 * 4

