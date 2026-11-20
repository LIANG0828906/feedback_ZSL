from __future__ import annotations

import math
import struct
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import E_PARSE_INVALID, E_VERSION_LOSS, ParseError, VersionLossError
from .model import Face, Mesh


@dataclass
class ExportReport:
    code: str = "OK"
    path: str = ""
    format: str = ""
    format_version: int = 2
    bytes_written: int = 0
    affected_faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "path": self.path,
            "format": self.format,
            "format_version": self.format_version,
            "bytes_written": self.bytes_written,
            "affected_faces": sorted(set(self.affected_faces)),
            "details": self.details,
        }


def _error(path: str | Path, message: str, line: int | None = None, offset: int | None = None) -> ParseError:
    return ParseError(
        code=E_PARSE_INVALID,
        message=message,
        file=str(path),
        line=line,
        offset=offset,
    )


def _fmt_float(value: float) -> str:
    value = float(value)
    if not math.isfinite(value):
        raise ValueError("cannot serialize a non-finite coordinate")
    if value == 0.0:
        return "0"
    return format(value, ".17g")


def _obj_index(token: str, total: int, *, path: str | Path, line: int, kind: str) -> int:
    try:
        raw = int(token)
    except ValueError as exc:
        raise _error(path, f"invalid {kind} index {token!r}", line=line) from exc
    if raw == 0:
        raise _error(path, f"{kind} index 0 is not valid in OBJ", line=line)
    idx = raw - 1 if raw > 0 else total + raw
    if idx < 0 or idx >= total:
        raise _error(path, f"{kind} index {raw} is out of range (count={total})", line=line)
    return idx


def parse_obj(path: str | Path) -> Mesh:
    path = Path(path)
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise _error(path, f"cannot read file: {exc}", offset=0) from exc
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise _error(path, "OBJ is not valid UTF-8", offset=exc.start) from exc

    vertices: list[list[float]] = []
    texcoords: list[tuple[float, ...]] = []
    normals: list[list[float]] = []
    faces: list[Face] = []
    current_group = "default"
    version = 1

    for line_no, raw_line in enumerate(text.splitlines(), start=1):
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith("#"):
            marker = line[1:].strip()
            if marker.startswith("mesh_editor:format_version"):
                parts = marker.split()
                if len(parts) != 2:
                    raise _error(path, "malformed mesh_editor format-version marker", line=line_no)
                try:
                    version = int(parts[1])
                except ValueError as exc:
                    raise _error(path, "format version must be an integer", line=line_no) from exc
                if version not in (1, 2):
                    raise _error(path, f"unsupported OBJ format version {version}", line=line_no)
            continue
        parts = line.split()
        tag = parts[0]
        if tag == "v":
            if len(parts) < 4:
                raise _error(path, "vertex requires x, y and z", line=line_no)
            try:
                xyz = [float(parts[1]), float(parts[2]), float(parts[3])]
            except ValueError as exc:
                raise _error(path, "vertex contains a non-numeric coordinate", line=line_no) from exc
            if not all(math.isfinite(v) for v in xyz):
                raise _error(path, "vertex contains a non-finite coordinate", line=line_no)
            vertices.append(xyz)
        elif tag == "vt":
            if len(parts) < 3:
                raise _error(path, "texture coordinate requires at least u and v", line=line_no)
            try:
                uv = tuple(float(x) for x in parts[1:])
            except ValueError as exc:
                raise _error(path, "texture coordinate contains a non-numeric value", line=line_no) from exc
            if len(uv) > 3 or not all(math.isfinite(v) for v in uv):
                raise _error(path, "invalid texture coordinate", line=line_no)
            texcoords.append(uv)
        elif tag == "vn":
            if len(parts) != 4:
                raise _error(path, "normal requires exactly x, y and z", line=line_no)
            try:
                n = [float(parts[1]), float(parts[2]), float(parts[3])]
            except ValueError as exc:
                raise _error(path, "normal contains a non-numeric value", line=line_no) from exc
            if not all(math.isfinite(v) for v in n):
                raise _error(path, "normal contains a non-finite value", line=line_no)
            normals.append(n)
        elif tag == "g":
            group = " ".join(parts[1:]).strip() or "default"
            if "\n" in group or "\r" in group:
                raise _error(path, "invalid group name", line=line_no)
            current_group = group
        elif tag == "f":
            corners = parts[1:]
            if len(corners) < 3:
                raise _error(path, "face requires at least three corners", line=line_no)
            v_idx: list[int] = []
            t_idx: list[int] = []
            n_idx: list[int] = []
            has_t = False
            has_n = False
            for corner in corners:
                refs = corner.split("/")
                if len(refs) > 3 or not refs[0]:
                    raise _error(path, f"malformed face corner {corner!r}", line=line_no)
                v_idx.append(_obj_index(refs[0], len(vertices), path=path, line=line_no, kind="vertex"))
                if len(refs) >= 2 and refs[1]:
                    t_idx.append(
                        _obj_index(refs[1], len(texcoords), path=path, line=line_no, kind="texture coordinate")
                    )
                    has_t = True
                elif len(refs) >= 2:
                    t_idx.append(-1)
                if len(refs) >= 3 and refs[2]:
                    n_idx.append(_obj_index(refs[2], len(normals), path=path, line=line_no, kind="normal"))
                    has_n = True
                elif len(refs) >= 3:
                    n_idx.append(-1)
            if len(set(v_idx)) != len(v_idx):
                raise _error(path, "face repeats a vertex index", line=line_no)
            if has_t and any(i < 0 for i in t_idx):
                raise _error(path, "face mixes present and missing texture coordinates", line=line_no)
            if has_n and any(i < 0 for i in n_idx):
                raise _error(path, "face mixes present and missing normals", line=line_no)
            try:
                faces.append(
                    Face(
                        tuple(v_idx),
                        tuple(t_idx) if has_t else None,
                        tuple(n_idx) if has_n else None,
                        current_group,
                    )
                )
            except ValueError as exc:
                raise _error(path, str(exc), line=line_no) from exc
        elif tag in {"o", "s", "usemtl", "mtllib", "l", "p"}:
            continue
        else:
            raise _error(path, f"unsupported OBJ field {tag!r}", line=line_no)

    if not vertices:
        raise _error(path, "OBJ contains no vertices", line=1)
    if not faces:
        raise _error(path, "OBJ contains no faces", line=1)
    try:
        return Mesh(
            np.asarray(vertices, dtype=np.float64),
            faces,
            texcoords if texcoords else None,
            np.asarray(normals, dtype=np.float64) if normals else None,
            {"source_format": "obj", "format_version": version},
        )
    except ValueError as exc:
        raise _error(path, str(exc)) from exc


def obj_loss(mesh: Mesh, version: int) -> list[int]:
    if version == 2:
        return []
    if version != 1:
        raise ValueError("OBJ format version must be 1 or 2")
    return [i for i, face in enumerate(mesh.faces) if face.group != "default"]


def serialize_obj(mesh: Mesh, version: int = 2, *, strict: bool = True) -> bytes:
    if version not in (1, 2):
        raise ValueError("OBJ format version must be 1 or 2")
    lost = obj_loss(mesh, version)
    if lost and strict:
        raise VersionLossError(
            code=E_VERSION_LOSS,
            message="OBJ v1 cannot represent non-default face groups",
            faces=lost,
            details={"target_format": "obj", "target_version": 1, "attribute": "face_groups"},
        )
    lines = [
        "# Mesh Editor deterministic OBJ",
        f"# mesh_editor:format_version {version}",
    ]
    for v in mesh.vertices:
        lines.append(f"v {_fmt_float(v[0])} {_fmt_float(v[1])} {_fmt_float(v[2])}")
    if mesh.texcoords is not None:
        for uv in mesh.texcoords:
            lines.append("vt " + " ".join(_fmt_float(x) for x in uv))
    if mesh.normals is not None:
        for n in mesh.normals:
            lines.append(f"vn {_fmt_float(n[0])} {_fmt_float(n[1])} {_fmt_float(n[2])}")
    current_group: str | None = None
    for face in mesh.faces:
        if version == 2 and face.group != current_group:
            lines.append(f"g {face.group}")
            current_group = face.group
        corners: list[str] = []
        for i, vertex in enumerate(face.vertices):
            fields = [str(vertex + 1)]
            if face.texcoords is not None:
                fields.append(str(face.texcoords[i] + 1))
            elif face.normals is not None:
                fields.append("")
            if face.normals is not None:
                fields.append(str(face.normals[i] + 1))
            corners.append("/".join(fields))
        lines.append("f " + " ".join(corners))
    return ("\n".join(lines) + "\n").encode("utf-8")


def _stl_triangles(mesh: Mesh) -> list[tuple[int, np.ndarray, np.ndarray, np.ndarray]]:
    result: list[tuple[int, np.ndarray, np.ndarray, np.ndarray]] = []
    for face_id, a, b, c in mesh.triangle_iter():
        result.append((face_id, mesh.vertices[a], mesh.vertices[b], mesh.vertices[c]))
    return result


def stl_loss(mesh: Mesh, version: int) -> tuple[list[int], dict[str, Any]]:
    if version not in (1, 2):
        raise ValueError("STL format version must be 1 or 2")
    affected: set[int] = set()
    attributes: list[str] = []
    grouped = [i for i, f in enumerate(mesh.faces) if f.group != "default"]
    if grouped:
        affected.update(grouped)
        attributes.append("face_groups")
    if mesh.texcoords is not None and any(f.texcoords is not None for f in mesh.faces):
        affected.update(i for i, f in enumerate(mesh.faces) if f.texcoords is not None)
        attributes.append("texture_coordinates")
    vertex_use: dict[int, int] = {}
    for face in mesh.faces:
        for index in set(face.vertices):
            vertex_use[index] = vertex_use.get(index, 0) + 1
    shared_vertices = {index for index, uses in vertex_use.items() if uses > 1}
    if shared_vertices:
        affected.update(
            i
            for i, face in enumerate(mesh.faces)
            if any(index in shared_vertices for index in face.vertices)
        )
        attributes.append("vertex_sharing")
    # STL stores per-facet corners, so arbitrary polygon faces are triangulated too.
    if any(len(f.vertices) != 3 for f in mesh.faces):
        affected.update(i for i, f in enumerate(mesh.faces) if len(f.vertices) != 3)
        attributes.append("polygon_faces_triangulated")
    details = {
        "target_format": "stl",
        "target_version": version,
        "attributes": sorted(attributes),
    }
    return sorted(affected), details


def serialize_stl_ascii(mesh: Mesh, version: int = 2, *, strict: bool = True) -> bytes:
    lost, details = stl_loss(mesh, version)
    if lost and strict:
        raise VersionLossError(
            code=E_VERSION_LOSS,
            message="STL cannot represent every mesh attribute",
            faces=lost,
            details=details,
        )
    lines = [f"solid mesh_editor_v{version}"]
    for face_id, ia, ib, ic in mesh.triangle_iter():
        a = mesh.vertices[ia]
        b = mesh.vertices[ib]
        c = mesh.vertices[ic]
        n = np.cross(b - a, c - a)
        length = float(np.linalg.norm(n))
        if length > 1e-15:
            n = n / length
        else:
            n = np.zeros(3, dtype=np.float64)
        lines.append(
            "  facet normal " + " ".join(_fmt_float(x) for x in n)
        )
        lines.append("    outer loop")
        for p in (a, b, c):
            lines.append("      vertex " + " ".join(_fmt_float(x) for x in p))
        lines.append("    endloop")
        lines.append("  endfacet")
    lines.append(f"endsolid mesh_editor_v{version}")
    return ("\n".join(lines) + "\n").encode("utf-8")


def serialize_stl_binary(mesh: Mesh, version: int = 2, *, strict: bool = True) -> bytes:
    lost, details = stl_loss(mesh, version)
    if lost and strict:
        raise VersionLossError(
            code=E_VERSION_LOSS,
            message="STL cannot represent every mesh attribute",
            faces=lost,
            details=details,
        )
    triangles = _stl_triangles(mesh)
    header_text = f"MESH_EDITOR_STL_V{version}".encode("ascii")
    header = header_text + b"\0" * (80 - len(header_text))
    chunks = [header, struct.pack("<I", len(triangles))]
    for _, a, b, c in triangles:
        n = np.cross(b - a, c - a)
        length = float(np.linalg.norm(n))
        if length > 1e-15:
            n = n / length
        else:
            n = np.zeros(3, dtype=np.float64)
        chunks.append(
            struct.pack(
                "<12fH",
                *(float(x) for x in n),
                *(float(x) for x in a),
                *(float(x) for x in b),
                *(float(x) for x in c),
                0,
            )
        )
    return b"".join(chunks)


def parse_stl(path: str | Path) -> Mesh:
    path = Path(path)
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise _error(path, f"cannot read file: {exc}", offset=0) from exc
    if len(raw) < 15:
        raise _error(path, "STL is too short", offset=0)

    looks_ascii = raw.lstrip().startswith(b"solid") and (b"facet" in raw[:4096] or b"endsolid" in raw)
    if not looks_ascii:
        return _parse_stl_binary(path, raw)
    return _parse_stl_ascii(path, raw)


def _parse_stl_binary(path: Path, raw: bytes) -> Mesh:
    if len(raw) < 84:
        raise _error(path, "binary STL header is truncated", offset=0)
    count = struct.unpack_from("<I", raw, 80)[0]
    expected = 84 + 50 * count
    if expected != len(raw):
        offset = min(len(raw), 84)
        raise _error(
            path,
            f"binary STL length mismatch: header declares {count} facets ({expected} bytes), file has {len(raw)}",
            offset=offset,
        )
    header = raw[:80]
    version = 1
    if header.startswith(b"MESH_EDITOR_STL_V2"):
        version = 2
    elif header.startswith(b"MESH_EDITOR_STL_V1"):
        version = 1
    vertices: list[tuple[float, float, float]] = []
    faces: list[Face] = []
    for i in range(count):
        base = 84 + i * 50
        values = struct.unpack_from("<12fH", raw, base)
        if not all(math.isfinite(x) for x in values[:12]):
            raise _error(path, f"facet {i} contains a non-finite value", offset=base)
        a = tuple(values[3:6])
        b = tuple(values[6:9])
        c = tuple(values[9:12])
        start = len(vertices)
        vertices.extend((a, b, c))
        faces.append(Face((start, start + 1, start + 2)))
    if not faces:
        raise _error(path, "STL contains no facets", offset=80)
    return Mesh(
        np.asarray(vertices, dtype=np.float64),
        faces,
        None,
        None,
        {"source_format": "stl", "format_version": version, "binary": True},
    )


def _parse_stl_ascii(path: Path, raw: bytes) -> Mesh:
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise _error(path, "ASCII STL is not valid UTF-8", offset=exc.start) from exc
    vertices: list[tuple[float, float, float]] = []
    faces: list[Face] = []
    in_facet = False
    in_loop = False
    facet_vertices: list[tuple[float, float, float]] = []
    version = 1
    for line_no, raw_line in enumerate(text.splitlines(), start=1):
        line = raw_line.strip()
        if not line:
            continue
        parts = line.split()
        tag = parts[0].lower()
        if tag == "solid":
            token = parts[1] if len(parts) > 1 else ""
            if token.lower() == "mesh_editor_v2":
                version = 2
            elif token.lower() == "mesh_editor_v1":
                version = 1
        elif tag == "facet":
            if in_facet:
                raise _error(path, "nested facet", line=line_no)
            if len(parts) != 5 or parts[1].lower() != "normal":
                raise _error(path, "facet requires exactly 'normal nx ny nz'", line=line_no)
            try:
                normal = [float(x) for x in parts[2:5]]
            except ValueError as exc:
                raise _error(path, "facet normal is not numeric", line=line_no) from exc
            if not all(math.isfinite(x) for x in normal):
                raise _error(path, "facet normal is not finite", line=line_no)
            in_facet = True
            facet_vertices = []
        elif tag == "outer":
            if not in_facet or len(parts) != 2 or parts[1].lower() != "loop":
                raise _error(path, "outer loop must appear inside a facet", line=line_no)
            in_loop = True
        elif tag == "vertex":
            if not in_facet or not in_loop:
                raise _error(path, "vertex must appear inside outer loop", line=line_no)
            if len(parts) != 4:
                raise _error(path, "vertex requires exactly x, y and z", line=line_no)
            try:
                vertex = (float(parts[1]), float(parts[2]), float(parts[3]))
            except ValueError as exc:
                raise _error(path, "vertex is not numeric", line=line_no) from exc
            if not all(math.isfinite(x) for x in vertex):
                raise _error(path, "vertex is not finite", line=line_no)
            facet_vertices.append(vertex)
        elif tag == "endloop":
            if not in_loop:
                raise _error(path, "endloop without outer loop", line=line_no)
            if len(facet_vertices) != 3:
                raise _error(path, f"facet requires exactly three vertices, found {len(facet_vertices)}", line=line_no)
            in_loop = False
        elif tag == "endfacet":
            if not in_facet or in_loop:
                raise _error(path, "endfacet outside a complete facet", line=line_no)
            start = len(vertices)
            vertices.extend(facet_vertices)
            faces.append(Face((start, start + 1, start + 2)))
            in_facet = False
        elif tag == "endsolid":
            if in_facet or in_loop:
                raise _error(path, "endsolid before facet completion", line=line_no)
        elif tag == "color":
            continue
        else:
            raise _error(path, f"unsupported ASCII STL field {parts[0]!r}", line=line_no)
    if in_facet or in_loop:
        raise _error(path, "ASCII STL ends inside a facet", line=len(text.splitlines()))
    if not faces:
        raise _error(path, "ASCII STL contains no facets", line=1)
    return Mesh(
        np.asarray(vertices, dtype=np.float64),
        faces,
        None,
        None,
        {"source_format": "stl", "format_version": version, "binary": False},
    )


def loss_report(mesh: Mesh, fmt: str, version: int) -> tuple[list[int], dict[str, Any]]:
    fmt = fmt.lower()
    if fmt == "obj":
        lost = obj_loss(mesh, version)
        return lost, {
            "target_format": "obj",
            "target_version": version,
            "attributes": ["face_groups"] if lost else [],
        }
    if fmt == "stl":
        return stl_loss(mesh, version)
    raise ValueError(f"unsupported mesh format {fmt!r}")


def serialize_mesh(mesh: Mesh, fmt: str, version: int = 2, allow_lossy: bool = False) -> bytes:
    fmt = fmt.lower()
    lost, details = loss_report(mesh, fmt, version)
    if lost and not allow_lossy:
        raise VersionLossError(
            code=E_VERSION_LOSS,
            message=f"{fmt.upper()} v{version} cannot represent every mesh attribute",
            faces=lost,
            details=details,
        )
    if fmt == "obj":
        return serialize_obj(mesh, version, strict=False)
    if fmt == "stl":
        return serialize_stl_binary(mesh, version, strict=False)
    raise ValueError(f"unsupported mesh format {fmt!r}")


def parse_mesh(path: str | Path, fmt: str | None = None) -> Mesh:
    path = Path(path)
    chosen = (fmt or path.suffix.lstrip(".")).lower()
    if chosen == "obj":
        return parse_obj(path)
    if chosen == "stl":
        return parse_stl(path)
    raise ValueError(f"unsupported mesh format {chosen!r}")


def export_mesh(
    mesh: Mesh,
    path: str | Path,
    fmt: str,
    version: int = 2,
    *,
    ascii_stl: bool = False,
    allow_lossy: bool = False,
) -> ExportReport:
    fmt = fmt.lower()
    path = Path(path)
    lost, details = loss_report(mesh, fmt, version)
    if lost and not allow_lossy:
        raise VersionLossError(
            code=E_VERSION_LOSS,
            message=f"{fmt.upper()} v{version} cannot represent every mesh attribute",
            faces=lost,
            details=details,
            file=str(path),
        )
    if fmt == "obj":
        payload = serialize_obj(mesh, version, strict=False)
    elif fmt == "stl":
        payload = (
            serialize_stl_ascii(mesh, version, strict=False)
            if ascii_stl
            else serialize_stl_binary(mesh, version, strict=False)
        )
    else:
        raise ValueError(f"unsupported mesh format {fmt!r}")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(payload)
    return ExportReport(
        code=E_VERSION_LOSS if lost else "OK",
        path=str(path),
        format=fmt,
        format_version=version,
        bytes_written=len(payload),
        affected_faces=lost,
        details=details,
    )
