from __future__ import annotations

import json
import math
import os
import tempfile
from pathlib import Path
from typing import Any, Sequence

from .errors import (
    E_PARSE_INVALID,
    E_SHARE_INVALID,
    PipelineError,
)
from .model import (
    Contour,
    PathData,
    Point,
    Segment,
    VertexRef,
    VertexShare,
)


DEFAULT_V1_TOLERANCE = 1e-6
FORMAT_NAME = "vector-path"
VALID_FILL_RULES = {"nonzero", "evenodd"}


def _require_mapping(value: Any, what: str, **details: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise PipelineError(E_PARSE_INVALID, f"{what} must be an object", **details)
    return value


def _require_string(value: Any, what: str, **details: Any) -> str:
    if not isinstance(value, str) or not value:
        raise PipelineError(E_PARSE_INVALID, f"{what} must be a non-empty string", **details)
    return value


def _number(value: Any, what: str, **details: Any) -> float:
    if (
        not isinstance(value, (int, float))
        or isinstance(value, bool)
        or not math.isfinite(value)
    ):
        raise PipelineError(E_PARSE_INVALID, f"{what} must be finite", **details)
    return float(value)


def _parse_segment(
    record: Any,
    contour_index: int,
    segment_index: int,
    start: Point,
) -> Segment:
    data = _require_mapping(
        record,
        "segment",
        contour_index=contour_index,
        segment_index=segment_index,
    )
    kind = data.get("type")
    details = {"contour_index": contour_index, "segment_index": segment_index}
    if kind not in {"line", "quad", "cubic"}:
        raise PipelineError(E_PARSE_INVALID, "unknown segment type", **details)
    if "to" not in data:
        raise PipelineError(E_PARSE_INVALID, "segment is missing 'to'", **details)
    end = Point.from_json(data["to"])
    if kind == "line":
        segment = Segment.line(start, end)
    elif kind == "quad":
        if "control" not in data:
            raise PipelineError(E_PARSE_INVALID, "quadratic segment missing control", **details)
        segment = Segment.quadratic(start, Point.from_json(data["control"]), end)
    else:
        if "control1" not in data or "control2" not in data:
            raise PipelineError(
                E_PARSE_INVALID,
                "cubic segment requires control1 and control2",
                **details,
            )
        segment = Segment.cubic(
            start,
            Point.from_json(data["control1"]),
            Point.from_json(data["control2"]),
            end,
        )
    segment.validate()
    trace_records = data.get("trace", [])
    if trace_records:
        from .model import EdgeTrace

        traces: list[EdgeTrace] = []
        if not isinstance(trace_records, list):
            raise PipelineError(E_PARSE_INVALID, "trace must be a list", **details)
        for trace in trace_records:
            trace_data = _require_mapping(trace, "trace entry", **details)
            t_values = trace_data.get("t", [0.0, 1.0])
            if not isinstance(t_values, list) or len(t_values) != 2:
                raise PipelineError(E_PARSE_INVALID, "trace t must have two values", **details)
            traces.append(
                EdgeTrace(
                    operand=str(trace_data.get("operand", "input")),
                    contour_index=int(trace_data.get("contour_index", contour_index)),
                    contour_id=str(trace_data.get("contour_id", "")),
                    segment_index=int(trace_data.get("segment_index", segment_index)),
                    subsegment_index=int(trace_data.get("subsegment_index", 0)),
                    t_start=float(t_values[0]),
                    t_end=float(t_values[1]),
                    source_kind=str(trace_data.get("source", "input")),
                )
            )
        segment = segment.with_trace(traces)
    return segment


def _parse_contour(record: Any, contour_index: int) -> Contour:
    data = _require_mapping(record, "contour", contour_index=contour_index)
    contour_id = data.get("id", f"contour-{contour_index}")
    if not isinstance(contour_id, str) or not contour_id:
        raise PipelineError(
            E_PARSE_INVALID,
            "contour id must be a non-empty string",
            contour_index=contour_index,
        )
    if "start" not in data:
        raise PipelineError(
            E_PARSE_INVALID,
            "contour is missing start",
            contour_index=contour_index,
            contour_id=contour_id,
        )
    records = data.get("segments")
    if not isinstance(records, list) or not records:
        raise PipelineError(
            E_PARSE_INVALID,
            "contour must have at least one segment",
            contour_index=contour_index,
            contour_id=contour_id,
        )
    start = Point.from_json(data["start"])
    current = start
    segments: list[Segment] = []
    for segment_index, segment_record in enumerate(records):
        segment = _parse_segment(segment_record, contour_index, segment_index, current)
        segments.append(segment)
        current = segment.end
    return Contour(
        segments=segments,
        contour_id=contour_id,
        direction=str(data.get("direction", "")),
        group=data.get("group"),
        direction_source=str(data.get("direction_source", "explicit")),
        orientation_corrected=bool(data.get("orientation_corrected", False)),
        metadata=dict(data.get("metadata", {})),
    )


def _validate_groups(contours: Sequence[Contour], groups: Sequence[str]) -> None:
    group_set = set(groups)
    for index, contour in enumerate(contours):
        if contour.group is not None:
            if not isinstance(contour.group, str):
                raise PipelineError(
                    E_PARSE_INVALID,
                    "group must be a string or null",
                    contour_index=index,
                    contour_id=contour.contour_id,
                    field="group",
                )
            if contour.group not in group_set:
                raise PipelineError(
                    E_PARSE_INVALID,
                    "contour references an unknown group",
                    contour_index=index,
                    contour_id=contour.contour_id,
                    field="group",
                    group=contour.group,
                )


def _parse_shares(
    records: Any,
    contours: Sequence[Contour],
    tolerance: float,
) -> list[VertexShare]:
    if not isinstance(records, list):
        raise PipelineError(E_SHARE_INVALID, "vertex_shares must be a list")
    shares: list[VertexShare] = []
    seen_refs: set[tuple[int, int]] = set()
    for share_index, record in enumerate(records):
        data = _require_mapping(record, "vertex share", share_index=share_index)
        if "point" not in data or "refs" not in data:
            raise PipelineError(
                E_SHARE_INVALID,
                "vertex share requires point and refs",
                share_index=share_index,
            )
        point = Point.from_json(data["point"])
        refs_data = data["refs"]
        if not isinstance(refs_data, list) or not refs_data:
            raise PipelineError(
                E_SHARE_INVALID,
                "vertex share refs must be a non-empty list",
                share_index=share_index,
            )
        refs: list[VertexRef] = []
        for ref_record in refs_data:
            ref_data = _require_mapping(ref_record, "vertex ref", share_index=share_index)
            contour_index = ref_data.get("contour")
            vertex_index = ref_data.get("vertex")
            if (
                not isinstance(contour_index, int)
                or not 0 <= contour_index < len(contours)
            ):
                raise PipelineError(
                    E_SHARE_INVALID,
                    "vertex share references a missing contour",
                    share_index=share_index,
                    contour_index=contour_index,
                )
            contour = contours[contour_index]
            vertex_count = len(contour.vertices)
            if (
                not isinstance(vertex_index, int)
                or not 0 <= vertex_index < vertex_count
            ):
                raise PipelineError(
                    E_SHARE_INVALID,
                    "vertex share index does not match contour vertex count",
                    share_index=share_index,
                    contour_index=contour_index,
                    contour_id=contour.contour_id,
                    vertex_index=vertex_index,
                    vertex_count=vertex_count,
                )
            ref_key = (contour_index, vertex_index)
            if ref_key in seen_refs:
                raise PipelineError(
                    E_SHARE_INVALID,
                    "vertex is listed in more than one share",
                    share_index=share_index,
                    contour_index=contour_index,
                    vertex_index=vertex_index,
                )
            seen_refs.add(ref_key)
            actual = contour.vertices[vertex_index]
            if actual.distance_to(point) > tolerance:
                raise PipelineError(
                    E_SHARE_INVALID,
                    "shared vertex coordinate does not match contour vertex",
                    share_index=share_index,
                    contour_index=contour_index,
                    contour_id=contour.contour_id,
                    vertex_index=vertex_index,
                    expected=point.as_list(),
                    actual=actual.as_list(),
                )
            refs.append(VertexRef(contour_index, vertex_index))
        shares.append(VertexShare(point, tuple(refs), "explicit"))
    return shares


def parse_path(payload: Any, source: str | None = None) -> PathData:
    data = _require_mapping(payload, "path document")
    format_name = data.get("format", FORMAT_NAME)
    if format_name != FORMAT_NAME:
        raise PipelineError(E_PARSE_INVALID, "unsupported path format", format=format_name)
    version = data.get("version")
    if version not in (1, 2):
        raise PipelineError(E_PARSE_INVALID, "version must be 1 or 2", version=version)
    fill_rule = data.get("fill_rule", "nonzero")
    if fill_rule not in VALID_FILL_RULES:
        raise PipelineError(E_PARSE_INVALID, "unsupported fill rule", fill_rule=fill_rule)
    records = data.get("contours")
    if not isinstance(records, list):
        raise PipelineError(E_PARSE_INVALID, "document must contain a contours array")
    if not records and not data.get("empty_result", False):
        raise PipelineError(
            E_PARSE_INVALID,
            "document must contain contours unless empty_result is declared",
        )
    contours = [_parse_contour(record, index) for index, record in enumerate(records)]

    if version == 1:
        tolerance = DEFAULT_V1_TOLERANCE
        tolerance_source = "v1_default_undeclared"
        groups: list[str] = []
        shares: list[VertexShare] = []
    else:
        if "tolerance" not in data:
            raise PipelineError(E_PARSE_INVALID, "v2 requires a tolerance declaration")
        tolerance = _number(data["tolerance"], "tolerance")
        if tolerance <= 0.0:
            raise PipelineError(E_PARSE_INVALID, "tolerance must be positive", tolerance=tolerance)
        tolerance_source = "v2_declared"
        groups_data = data.get("groups", [])
        if not isinstance(groups_data, list) or not all(
            isinstance(item, str) and item for item in groups_data
        ):
            raise PipelineError(E_PARSE_INVALID, "groups must be an array of names")
        if len(set(groups_data)) != len(groups_data):
            raise PipelineError(E_PARSE_INVALID, "group names must be unique")
        groups = list(groups_data)
        for index, contour in enumerate(contours):
            direction = data["contours"][index].get("direction")
            if direction not in {"ccw", "cw"}:
                raise PipelineError(
                    E_PARSE_INVALID,
                    "v2 contour requires explicit direction",
                    contour_index=index,
                    contour_id=contour.contour_id,
                    field="direction",
                )
            contour.direction = direction
            contour.direction_source = "v2_explicit"
            if "group" not in data["contours"][index]:
                raise PipelineError(
                    E_PARSE_INVALID,
                    "v2 contour requires explicit group field",
                    contour_index=index,
                    contour_id=contour.contour_id,
                    field="group",
                )
        if "vertex_shares" not in data:
            raise PipelineError(E_PARSE_INVALID, "v2 requires vertex_shares")
        shares = _parse_shares(data["vertex_shares"], contours, tolerance)
        _validate_groups(contours, groups)

    closure_tolerance = tolerance
    for index, contour in enumerate(contours):
        if not contour.segments:
            raise PipelineError(
                E_PARSE_INVALID,
                "empty contour",
                contour_index=index,
                contour_id=contour.contour_id,
            )
        start = contour.segments[0].start
        end = contour.segments[-1].end
        gap = start.distance_to(end)
        if gap > closure_tolerance:
            raise PipelineError(
                E_PARSE_INVALID,
                "subcontour is not closed",
                contour_index=index,
                contour_id=contour.contour_id,
                gap=gap,
                tolerance=closure_tolerance,
            )
        if gap > 0.0:
            last = contour.segments[-1]
            contour.segments[-1] = Segment(
                last.kind,
                last.start,
                start,
                last.control1,
                last.control2,
                last.trace,
            )

    if version == 2:
        from .geometry import actual_direction

        for index, contour in enumerate(contours):
            observed = actual_direction(contour, tolerance)
            if observed != contour.direction:
                raise PipelineError(
                    E_PARSE_INVALID,
                    "explicit direction conflicts with vertex order",
                    contour_index=index,
                    contour_id=contour.contour_id,
                    field="direction",
                    declared=contour.direction,
                    observed=observed,
                )

    metadata_data = data.get("metadata", {})
    if not isinstance(metadata_data, dict):
        raise PipelineError(E_PARSE_INVALID, "path metadata must be an object")
    path = PathData(
        version=version,
        fill_rule=fill_rule,
        tolerance=tolerance,
        contours=contours,
        tolerance_source=tolerance_source,
        groups=groups,
        vertex_shares=shares,
        metadata=dict(metadata_data),
    )
    if version == 1:
        from .compat import infer_v1_metadata

        path = infer_v1_metadata(path)
    return path


def load_path(path: str | os.PathLike[str]) -> PathData:
    file_path = Path(path)
    try:
        text = file_path.read_text(encoding="utf-8")
        payload = json.loads(text)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise PipelineError(
            E_PARSE_INVALID,
            "unable to read path JSON",
            source=str(file_path),
            error=str(exc),
        ) from exc
    return parse_path(payload, str(file_path))


def path_to_dict(path: PathData) -> dict[str, Any]:
    contours: list[dict[str, Any]] = []
    for contour in path.contours:
        record = contour.to_record(include_metadata=True)
        contours.append(record)
    return {
        "format": FORMAT_NAME,
        "version": path.version,
        "fill_rule": path.fill_rule,
        "tolerance": path.tolerance,
        "tolerance_source": path.tolerance_source,
        "empty_result": not path.contours,
        "groups": list(path.groups),
        "contours": contours,
        "vertex_shares": [share.to_dict() for share in path.vertex_shares],
        "metadata": path.metadata,
    }


def serialize_v2(path: PathData) -> dict[str, Any]:
    from .compat import derive_vertex_shares

    working = path.copy()
    working.version = 2
    working.tolerance_source = (
        "v2_declared" if path.version == 2 else "upgraded_from_v1"
    )
    if not working.vertex_shares:
        working.vertex_shares = derive_vertex_shares(working, "coordinate_derivation")
    payload = path_to_dict(working)
    payload["version"] = 2
    payload["tolerance"] = working.tolerance
    payload["tolerance_source"] = working.tolerance_source
    return payload


def serialize_v1(path: PathData) -> tuple[dict[str, Any], "VersionReport"]:
    from .compat import VersionReport, build_v1_loss_report

    if path.version == 2:
        report = build_v1_loss_report(path)
        payload = {
            "format": FORMAT_NAME,
            "version": 1,
            "fill_rule": path.fill_rule,
            "contours": [
                {
                    "id": contour.contour_id,
                    "start": contour.segments[0].start.as_list(),
                    "segments": [
                        _strip_trace(segment.to_record()) for segment in contour.segments
                    ],
                }
                for contour in path.contours
            ],
        }
        if not payload["contours"]:
            payload["empty_result"] = True
        return payload, report
    payload = {
        "format": FORMAT_NAME,
        "version": 1,
        "fill_rule": path.fill_rule,
        "contours": [
            {
                "id": contour.contour_id,
                "start": contour.segments[0].start.as_list(),
                "segments": [
                    _strip_trace(segment.to_record()) for segment in contour.segments
                ],
            }
            for contour in path.contours
        ],
    }
    if not payload["contours"]:
        payload["empty_result"] = True
    return payload, VersionReport()


def _strip_trace(record: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in record.items() if key != "trace"}


def export_path(
    path: PathData,
    version: int,
    destination: str | os.PathLike[str] | None = None,
) -> tuple[dict[str, Any], "VersionReport"]:
    from .compat import downgrade_to_v1

    if version == 2:
        from .compat import VersionReport

        payload = serialize_v2(path)
        report = VersionReport()
    elif version == 1:
        payload, report = downgrade_to_v1(path)
    else:
        raise PipelineError(E_PARSE_INVALID, "export version must be 1 or 2", version=version)
    if destination is not None:
        atomic_write_json(destination, payload)
    return payload, report


def atomic_write_json(
    destination: str | os.PathLike[str],
    payload: Any,
) -> None:
    path = Path(destination)
    path.parent.mkdir(parents=True, exist_ok=True)
    encoded = (
        json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    ).encode("utf-8")
    fd, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    except Exception:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def _fsync_directory(directory: Path) -> None:
    if os.name == "nt":
        return
    try:
        descriptor = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def dumps_path(path: PathData, version: int = 2) -> str:
    payload, _ = export_path(path, version)
    return json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2)

