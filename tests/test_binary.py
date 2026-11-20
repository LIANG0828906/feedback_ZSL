from __future__ import annotations

import struct
from pathlib import Path

import numpy as np
import pytest

from mesh_repair.binary import (
    HEADER_STRUCT,
    MAGIC,
    load_mesh,
    write_mesh,
)
from mesh_repair.errors import (
    E_MESH_INVALID,
    SUB_DEGENERATE_FACE,
    SUB_FACE_COUNT_MISMATCH,
    SUB_INDEX_OUT_OF_RANGE,
    SUB_NONFINITE_COORD,
)
from mesh_repair.mesh import Mesh
from mesh_repair.samples import build_scan_sample


def _raw_file(path: Path, vertices: np.ndarray, faces: np.ndarray) -> None:
    header = HEADER_STRUCT.pack(
        MAGIC,
        1,
        1,
        4,
        int(vertices.shape[0]),
        int(faces.shape[0]),
    )
    path.write_bytes(
        header
        + np.asarray(vertices, dtype="<f8").tobytes()
        + np.asarray(faces, dtype="<u4").tobytes()
    )


def test_header_roundtrip_and_coordinate_widths(tmp_path: Path) -> None:
    mesh = build_scan_sample()
    path = tmp_path / "mesh.msh"
    digest = write_mesh(path, mesh, coord_type="f32", index_width=2)
    loaded, header = load_mesh(path)
    assert header.coord_type == "f32"
    assert header.index_width == 2
    assert loaded.vertex_count == mesh.vertex_count
    assert loaded.face_count == mesh.face_count
    assert len(digest) == 64


def test_index_out_of_range_reports_first_face(tmp_path: Path) -> None:
    path = tmp_path / "bad-index.msh"
    _raw_file(
        path,
        np.asarray([[0, 0, 0], [1, 0, 0], [0, 1, 0], [0, 0, 1]], dtype=np.float64),
        np.asarray([[0, 1, 2], [0, 1, 9], [1, 2, 3]], dtype=np.uint32),
    )
    with pytest.raises(Exception) as caught:
        load_mesh(path)
    error = caught.value
    assert error.code == E_MESH_INVALID
    assert error.subcode == SUB_INDEX_OUT_OF_RANGE
    assert error.location["face"] == 1


def test_degenerate_face_reports_face(tmp_path: Path) -> None:
    path = tmp_path / "degenerate.msh"
    _raw_file(
        path,
        np.asarray([[0, 0, 0], [1, 0, 0], [0, 1, 0]], dtype=np.float64),
        np.asarray([[0, 1, 2], [1, 1, 2]], dtype=np.uint32),
    )
    with pytest.raises(Exception) as caught:
        load_mesh(path)
    assert caught.value.code == E_MESH_INVALID
    assert caught.value.subcode == SUB_DEGENERATE_FACE
    assert caught.value.location["face"] == 1


def test_nonfinite_coordinate_reports_vertex(tmp_path: Path) -> None:
    path = tmp_path / "nonfinite.msh"
    _raw_file(
        path,
        np.asarray([[0, 0, 0], [1, 0, 0], [np.nan, 1, 0]], dtype=np.float64),
        np.asarray([[0, 1, 2]], dtype=np.uint32),
    )
    with pytest.raises(Exception) as caught:
        load_mesh(path)
    assert caught.value.code == E_MESH_INVALID
    assert caught.value.subcode == SUB_NONFINITE_COORD
    assert caught.value.location["vertex"] == 2


def test_face_count_mismatch_is_rejected(tmp_path: Path) -> None:
    path = tmp_path / "mismatch.msh"
    _raw_file(
        path,
        np.asarray([[0, 0, 0], [1, 0, 0], [0, 1, 0]], dtype=np.float64),
        np.asarray([[0, 1, 2]], dtype=np.uint32),
    )
    path.write_bytes(path.read_bytes() + b"\0\0\0\0")
    with pytest.raises(Exception) as caught:
        load_mesh(path)
    assert caught.value.code == E_MESH_INVALID
    assert caught.value.subcode == SUB_FACE_COUNT_MISMATCH


def test_writer_rejects_invalid_mesh(tmp_path: Path) -> None:
    mesh = Mesh(
        np.asarray([[0, 0, 0], [1, 0, 0], [0, 1, 0]], dtype=np.float64),
        np.asarray([[0, 1, 99]], dtype=np.int64),
    )
    with pytest.raises(Exception) as caught:
        write_mesh(tmp_path / "invalid.msh", mesh)
    assert caught.value.subcode == SUB_INDEX_OUT_OF_RANGE
