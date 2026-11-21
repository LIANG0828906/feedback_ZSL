"""Deterministic serialization, hashing and atomic file commits."""

import hashlib
import json
import os

import numpy as np


def to_py(obj):
    """Recursively convert numpy types to native Python types."""
    if isinstance(obj, np.ndarray):
        return [to_py(v) for v in obj.tolist()]
    if isinstance(obj, (np.floating, np.integer)):
        return obj.item()
    if isinstance(obj, dict):
        return {k: to_py(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [to_py(v) for v in obj]
    return obj


def canonical_json(obj):
    """Deterministic JSON text: sorted keys, tight separators, no timestamps."""
    return json.dumps(to_py(obj), sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False)


def content_hash(obj):
    return hashlib.sha256(canonical_json(obj).encode("utf-8")).hexdigest()


def hash_text(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def hash_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def atomic_write_text(path, text):
    """Write via temp file + atomic rename (checkpoint commit protocol)."""
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(text)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def write_json(path, obj):
    atomic_write_text(path, json.dumps(to_py(obj), indent=2, sort_keys=True,
                                       ensure_ascii=False) + "\n")


def read_json(path):
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)
