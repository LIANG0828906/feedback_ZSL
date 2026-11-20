from __future__ import annotations

import struct
import tempfile
import unittest
from pathlib import Path

from mesh_editor.errors import E_PARSE_INVALID, E_VERSION_LOSS, ParseError, VersionLossError
from mesh_editor.formats import (
    export_mesh,
    parse_mesh,
    parse_obj,
    parse_stl,
    serialize_obj,
    serialize_stl_ascii,
    serialize_stl_binary,
)
from mesh_editor.samplegen import build_demo_mesh


class FormatTests(unittest.TestCase):
    def test_obj_v2_preserves_attributes_and_groups(self) -> None:
        mesh = build_demo_mesh()
        payload = serialize_obj(mesh, 2)
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "part.obj"
            path.write_bytes(payload)
            loaded = parse_obj(path)
        self.assertEqual(len(loaded.vertices), len(mesh.vertices))
        self.assertEqual(len(loaded.faces), len(mesh.faces))
        self.assertIsNotNone(loaded.texcoords)
        self.assertIsNotNone(loaded.normals)
        self.assertEqual({f.group for f in loaded.faces}, {f.group for f in mesh.faces})
        self.assertEqual(loaded.faces[0].texcoords, mesh.faces[0].texcoords)
        self.assertEqual(loaded.faces[0].normals, mesh.faces[0].normals)
        self.assertEqual(loaded.metadata["format_version"], 2)

    def test_obj_v1_loss_is_reported_with_face_ids(self) -> None:
        mesh = build_demo_mesh()
        with self.assertRaises(VersionLossError) as caught:
            serialize_obj(mesh, 1)
        error = caught.exception
        self.assertEqual(error.code, E_VERSION_LOSS)
        self.assertEqual(error.faces, [i for i, f in enumerate(mesh.faces) if f.group != "default"])
        with tempfile.TemporaryDirectory() as temp:
            target = Path(temp) / "lossy.obj"
            with self.assertRaises(VersionLossError):
                export_mesh(mesh, target, "obj", 1)
            self.assertFalse(target.exists())

    def test_obj_v1_round_trip_and_upgrade(self) -> None:
        mesh = build_demo_mesh()
        ungrouped = type(mesh)(
            mesh.vertices,
            [type(f)(f.vertices, f.texcoords, f.normals, "default") for f in mesh.faces],
            mesh.texcoords,
            mesh.normals,
        )
        payload = serialize_obj(ungrouped, 1)
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "old.obj"
            path.write_bytes(payload)
            loaded = parse_obj(path)
        self.assertEqual(loaded.metadata["format_version"], 1)
        self.assertEqual(len(loaded.faces), len(ungrouped.faces))
        self.assertEqual({f.group for f in loaded.faces}, {"default"})
        upgraded = serialize_obj(loaded, 2)
        self.assertIn(b"# mesh_editor:format_version 2", upgraded)

    def test_ascii_and_binary_stl_round_trip(self) -> None:
        mesh = build_demo_mesh()
        for binary in (False, True):
            payload = serialize_stl_binary(mesh, 2, strict=False) if binary else serialize_stl_ascii(
                mesh, 2, strict=False
            )
            with tempfile.TemporaryDirectory() as temp:
                path = Path(temp) / "part.stl"
                path.write_bytes(payload)
                loaded = parse_stl(path)
            self.assertEqual(len(loaded.faces), sum(len(f.vertices) - 2 for f in mesh.faces))
            self.assertEqual(loaded.metadata["format_version"], 2)
            self.assertEqual(bool(loaded.metadata["binary"]), binary)
            self.assertEqual(len(loaded.vertices), len(loaded.faces) * 3)

    def test_obj_parse_error_has_line(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "bad.obj"
            path.write_text("v 0 0 0\nv 1 0\nf 1 2 3\n", encoding="utf-8")
            with self.assertRaises(ParseError) as caught:
                parse_obj(path)
        self.assertEqual(caught.exception.code, E_PARSE_INVALID)
        self.assertEqual(caught.exception.line, 2)

    def test_binary_stl_length_mismatch_has_offset(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "bad.stl"
            payload = b"BROKEN".ljust(80, b"\0") + struct.pack("<I", 2) + b"\0" * 50
            path.write_bytes(payload)
            with self.assertRaises(ParseError) as caught:
                parse_stl(path)
        self.assertEqual(caught.exception.code, E_PARSE_INVALID)
        self.assertEqual(caught.exception.offset, 84)

    def test_ascii_stl_missing_field_has_line(self) -> None:
        text = """solid mesh_editor_v2
facet normal 0 0 1
outer loop
vertex 0 0 0
vertex 1 0 0
endloop
endfacet
endsolid mesh_editor_v2
"""
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "bad.stl"
            path.write_text(text, encoding="utf-8")
            with self.assertRaises(ParseError) as caught:
                parse_stl(path)
        self.assertEqual(caught.exception.line, 6)

    def test_parse_mesh_dispatch(self) -> None:
        mesh = build_demo_mesh()
        with tempfile.TemporaryDirectory() as temp:
            obj = Path(temp) / "x.obj"
            obj.write_bytes(serialize_obj(mesh, 2))
            stl = Path(temp) / "x.stl"
            stl.write_bytes(serialize_stl_binary(mesh, 2, strict=False))
            self.assertEqual(len(parse_mesh(obj).faces), len(mesh.faces))
            self.assertEqual(len(parse_mesh(stl).faces), len(mesh.faces))


if __name__ == "__main__":
    unittest.main()
