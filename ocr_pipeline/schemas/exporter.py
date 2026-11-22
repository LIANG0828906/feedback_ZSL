"""Multi-version exporter with E_LOSSY downgrade reporting.

Exports the internal result model to any supported target version. Fields the
target cannot express produce E_LOSSY warning records. Output bytes are
deterministic: sorted keys, fixed separators, quantized floats.
"""

from __future__ import annotations

import json
from typing import Any, Dict, List, Tuple

from ..errors import LossyWarning, VersionError
from ..model import SUPPORTED_VERSIONS, ImageResult
from . import v1, v2, v3


def _canonical_bytes(doc: Dict[str, Any]) -> bytes:
    text = json.dumps(doc, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return (text + "\n").encode("utf-8")


def export_document(
    result: ImageResult,
    target_version: str,
    *,
    precision: int = 6,
    rounding: str = "half_up",
) -> Tuple[Dict[str, Any], List[LossyWarning]]:
    if target_version == v1.VERSION:
        return v1.dump(result)
    if target_version == v2.VERSION:
        return v2.dump(result, precision=precision, rounding=rounding)
    if target_version == v3.VERSION:
        return v3.dump(result)
    raise VersionError(
        f"unsupported target version: {target_version!r}",
        field_path="target_version",
    )


def export_bytes(
    result: ImageResult,
    target_version: str,
    *,
    precision: int = 6,
    rounding: str = "half_up",
) -> Tuple[bytes, List[LossyWarning]]:
    doc, warnings = export_document(
        result, target_version, precision=precision, rounding=rounding
    )
    return _canonical_bytes(doc), warnings


__all__ = [
    "SUPPORTED_VERSIONS",
    "export_document",
    "export_bytes",
]
