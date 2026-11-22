"""Multi-version OCR result loader.

Detects the schema version, delegates to the version module, and maps the
document into the internal result model. Unknown versions or missing
required fields raise E_VER with the offending field path.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Dict, Union

from ..errors import VersionError
from ..model import ImageResult
from . import v1, v2, v3

_MODULES = (v1, v2, v3)


def detect_version(doc: Dict[str, Any]) -> str:
    for module in _MODULES:
        if module.detect(doc):
            return module.VERSION
    if "version" in doc:
        raise VersionError(
            f"unsupported schema version: {doc['version']!r}", field_path="version"
        )
    if "schema_version" in doc:
        raise VersionError(
            f"unsupported schema version: {doc['schema_version']!r}",
            field_path="schema_version",
        )
    if isinstance(doc.get("meta"), dict) and "schema" in doc["meta"]:
        raise VersionError(
            f"unsupported schema version: {doc['meta']['schema']!r}",
            field_path="meta.schema",
        )
    raise VersionError("no recognizable schema version marker", field_path="<root>")


def load_document(doc: Dict[str, Any], *, rounding: str = "half_up") -> ImageResult:
    version = detect_version(doc)
    if version == v1.VERSION:
        return v1.load(doc)
    if version == v2.VERSION:
        return v2.load(doc, rounding=rounding)
    return v3.load(doc)


def load_file(path: Union[str, Path], *, rounding: str = "half_up") -> ImageResult:
    with open(path, "r", encoding="utf-8") as fh:
        doc = json.load(fh)
    if not isinstance(doc, dict):
        raise VersionError(
            "top-level JSON value must be an object", field_path="<root>"
        )
    return load_document(doc, rounding=rounding)
