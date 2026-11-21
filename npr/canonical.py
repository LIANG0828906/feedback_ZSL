"""Canonical serialization and hashing helpers.

Determinism rules used everywhere in the pipeline:
* dict keys are emitted in sorted order;
* floats are rounded onto the fixed coordinate/value grid before hashing;
* separators are fixed (no whitespace) so output is byte stable across runs;
* no timestamp, PID, absolute path or file size ever enters a hash.
"""

import hashlib
import json

import numpy as np

from . import QUANT_DECIMALS


def to_jsonable(obj):
    """Convert numpy/dataclass-ish structures into plain JSON-able types."""
    if isinstance(obj, dict):
        return {str(k): to_jsonable(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [to_jsonable(v) for v in obj]
    if isinstance(obj, np.ndarray):
        return to_jsonable(obj.tolist())
    if isinstance(obj, (np.floating,)):
        return float(obj)
    if isinstance(obj, (np.integer,)):
        return int(obj)
    return obj


def canonical_bytes(obj, decimals=QUANT_DECIMALS):
    plain = _quantize(to_jsonable(obj), decimals)
    return json.dumps(
        plain,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def canonical_hash(obj, decimals=QUANT_DECIMALS):
    return hashlib.sha256(canonical_bytes(obj, decimals)).hexdigest()


def _quantize(obj, decimals):
    if isinstance(obj, dict):
        return {k: _quantize(v, decimals) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_quantize(v, decimals) for v in obj]
    if isinstance(obj, float):
        return round(obj, decimals)
    return obj


def write_json_atomic(path, obj):
    """Write JSON via temp-file + os.replace; no half-written target."""
    import os

    path = os.fspath(path)
    tmp = f"{path}.tmp"
    try:
        with open(tmp, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(json.dumps(obj, sort_keys=True, indent=2))
            fh.write("\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except OSError as exc:
        try:
            if os.path.exists(tmp):
                os.unlink(tmp)
        except OSError:
            pass
        raise exc


def read_json(path):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)
