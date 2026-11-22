"""Small deterministic and persistence helpers."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any

import numpy as np


def array_content_hash(array: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(array)
    digest = hashlib.sha256()
    digest.update(str(contiguous.dtype).encode("ascii"))
    digest.update(repr(contiguous.shape).encode("ascii"))
    digest.update(contiguous.tobytes())
    return digest.hexdigest()


def bytes_hash(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def json_bytes(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")


def npz_bytes(**arrays: np.ndarray) -> bytes:
    import io

    buffer = io.BytesIO()
    np.savez_compressed(buffer, **arrays)
    return buffer.getvalue()


def npz_load(payload: bytes) -> dict[str, np.ndarray]:
    import io

    with np.load(io.BytesIO(payload), allow_pickle=False) as archive:
        return {key: archive[key].copy() for key in archive.files}


def atomic_write_bytes(path: str | Path, payload: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name(f".{target.name}.{os.getpid()}.tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, target)


def atomic_write_json(path: str | Path, value: Any) -> None:
    atomic_write_bytes(path, json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True).encode("utf-8") + b"\n")


def check_writable_directory(path: str | Path) -> dict[str, Any]:
    target = Path(path)
    target.mkdir(parents=True, exist_ok=True)
    probe = target / f".write_probe_{os.getpid()}"
    try:
        probe.write_bytes(b"ok")
        size = probe.stat().st_size
        probe.unlink()
    except OSError as exc:
        from .errors import WorkbenchError

        raise WorkbenchError(
            "E_IO",
            "workspace is not writable",
            {"path": str(target), "reason": str(exc)},
        ) from exc
    return {"path": str(target.resolve()), "writable": True, "probe_bytes": size}
