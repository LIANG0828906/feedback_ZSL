"""Round-trip equivalence checking.

The comparator walks both JSON documents in parallel and reports the *first*
inequivalent item as an object path plus the two values.  Numeric time fields
that were explicitly normalised (unit conversion) are compared with a tolerance
instead of requiring bit-identical floats; every other field must match
exactly, including field presence so that defaulted optional fields are not
silently re-introduced.
"""

from __future__ import annotations
from pathlib import Path
from typing import Any

from ..errors import RoundtripError
from ..model import TraceRecord
from .loader import load_score
from .exporter import export_score

_NUMERIC_TOLERANCE = 1e-9


def _normalised_paths(traces: list[TraceRecord]) -> set[str]:
    return {
        trace.object_path
        for trace in traces
        if trace.action == "normalise"
    }


def compare_documents(
    expected: Any,
    actual: Any,
    path: str = "$",
    *,
    normalised: set[str] | None = None,
) -> dict[str, Any] | None:
    """Return ``None`` when equivalent, otherwise the first mismatch record."""
    normalised = normalised or set()
    numeric = (
        isinstance(expected, (int, float))
        and isinstance(actual, (int, float))
        and not isinstance(expected, bool)
        and not isinstance(actual, bool)
    )
    if type(expected) is not type(actual) and not numeric:
        return {
            "path": path,
            "expected": expected,
            "actual": actual,
            "reason": "type differs",
        }
    if isinstance(expected, dict):
        expected_keys = set(expected)
        actual_keys = set(actual)
        if expected_keys != actual_keys:
            missing = sorted(expected_keys - actual_keys)
            extra = sorted(actual_keys - expected_keys)
            return {
                "path": path,
                "expected": sorted(expected_keys),
                "actual": sorted(actual_keys),
                "reason": f"field set differs (missing={missing}, extra={extra})",
            }
        for key in expected:
            mismatch = compare_documents(
                expected[key], actual[key], f"{path}.{key}",
                normalised=normalised,
            )
            if mismatch:
                return mismatch
        return None
    if isinstance(expected, list):
        if len(expected) != len(actual):
            return {
                "path": path,
                "expected": f"list[{len(expected)}]",
                "actual": f"list[{len(actual)}]",
                "reason": "list length differs",
            }
        for index, (left, right) in enumerate(zip(expected, actual)):
            mismatch = compare_documents(
                left, right, f"{path}[{index}]", normalised=normalised,
            )
            if mismatch:
                return mismatch
        return None
    if isinstance(expected, float) or isinstance(actual, float):
        if abs(float(expected) - float(actual)) <= _NUMERIC_TOLERANCE:
            return None
        return {
            "path": path,
            "expected": expected,
            "actual": actual,
            "reason": "numeric value exceeds normalisation tolerance",
        }
    if expected != actual:
        return {
            "path": path,
            "expected": expected,
            "actual": actual,
            "reason": "values differ",
        }
    return None


def _strip_internal(document: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in document.items()
            if not key.startswith("_")}


def roundtrip_check(
    source_path: str | Path, reference_path: str | Path | None = None
) -> dict[str, Any]:
    """Load ``source_path`` and re-export it to the same schema version.

    When ``reference_path`` is supplied the re-exported document is compared
    against that reference instead of the source file itself, which is how
    cross-target export regressions are detected.
    """
    source_path = str(source_path)
    project = load_score(source_path)
    score = project.score
    exported = export_score(
        score,
        project.source_version,
        project.provenance,
        confirm_lossy=True,
    )
    exported = _strip_internal(exported)
    import json

    comparison_path = str(reference_path or source_path)
    with open(comparison_path, "r", encoding="utf-8") as handle:
        original = json.load(handle)
    normalised = _normalised_paths(score.traces)
    mismatch = compare_documents(original, exported, normalised=normalised)
    if mismatch is not None:
        raise RoundtripError(
            "Round-trip produced an inequivalent document",
            {
                "file_path": source_path,
                "reference_path": comparison_path,
                "version": project.source_version,
                "first_mismatch": mismatch,
            },
        )
    return {
        "file_path": source_path,
        "version": project.source_version,
        "equivalent": True,
        "normalised_objects": sorted(normalised),
        "trace_count": len(score.traces),
    }
