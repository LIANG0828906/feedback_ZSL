"""Versioned fingerprint file formats (v1/v2/v3)."""

from . import v1, v2, v3
from ..errors import E_FMT_INVALID, FpError

PARSERS = {1: v1, 2: v2, 3: v3}


def load_fingerprint(path):
    """Parse and validate a fingerprint file of any known version.

    Raises FpError(E_FMT_INVALID) with file/field detail on any problem;
    never returns an empty fingerprint.
    """
    import json

    try:
        with open(path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise FpError(E_FMT_INVALID, f"cannot read fingerprint file: {exc}",
                      file=path, field="$")
    if not isinstance(doc, dict):
        raise FpError(E_FMT_INVALID, "fingerprint root must be an object",
                      file=path, field="$")
    version = doc.get("format_version")
    if version not in PARSERS:
        raise FpError(E_FMT_INVALID,
                      f"unknown format_version: {version!r}",
                      file=path, field="format_version")
    return PARSERS[version].parse(doc, path)


def save_fingerprint(fp, path, version=3):
    """Serialize a unified Fingerprint to the requested on-disk version."""
    import json

    doc = PARSERS[version].serialize(fp)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2, sort_keys=True)
        fh.write("\n")
    return path
