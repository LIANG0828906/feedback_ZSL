from __future__ import annotations

import hashlib
import json
import os
import re
import tempfile
from pathlib import Path
from typing import Any

from .errors import ErrorCode, build_error

_SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")


def ensure_safe_id(value: str, field: str = "id") -> str:
    if not isinstance(value, str) or not _SAFE_ID.fullmatch(value):
        raise build_error(
            ErrorCode.CONFIG_INVALID,
            f"{field} must match [A-Za-z0-9][A-Za-z0-9_.-]{{0,63}}",
            field=field,
            value=value,
        )
    return value


def canonical_json_text(value: Any) -> str:
    return json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(",", ":"), allow_nan=False)


def canonical_json_bytes(value: Any) -> bytes:
    return canonical_json_text(value).encode("utf-8")


def pretty_json_text(value: Any) -> str:
    return json.dumps(value, ensure_ascii=True, sort_keys=True, indent=2, allow_nan=False) + "\n"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_text(text: str) -> str:
    return sha256_bytes(text.encode("utf-8"))


def sha256_obj(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def sha256_file(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def read_json(path: str | os.PathLike[str]) -> Any:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except json.JSONDecodeError as exc:
        raise build_error(
            ErrorCode.IO_ERROR,
            f"invalid JSON in {path}",
            path=str(path),
            line=exc.lineno,
            column=exc.colno,
        ) from exc
    except OSError as exc:
        raise build_error(ErrorCode.IO_ERROR, f"cannot read {path}", path=str(path), detail=str(exc)) from exc


def ensure_dir(path: str | os.PathLike[str]) -> Path:
    target = Path(path)
    try:
        target.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise build_error(ErrorCode.IO_ERROR, f"cannot create directory {path}", path=str(path), detail=str(exc)) from exc
    return target


def write_bytes_atomic(path: str | os.PathLike[str], data: bytes) -> None:
    """Write a sibling temp file, fsync it, then atomically replace the target."""
    target = Path(path)
    ensure_dir(target.parent)
    fd, temp_name = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
    except OSError as exc:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise build_error(ErrorCode.IO_ERROR, f"cannot atomically write {target}", path=str(target), detail=str(exc)) from exc


def write_json_atomic(path: str | os.PathLike[str], value: Any, pretty: bool = True) -> None:
    text = pretty_json_text(value) if pretty else canonical_json_text(value)
    write_bytes_atomic(path, text.encode("utf-8"))


def append_jsonl(path: str | os.PathLike[str], value: Any) -> None:
    target = Path(path)
    ensure_dir(target.parent)
    try:
        with open(target, "a", encoding="utf-8", newline="\n") as handle:
            handle.write(canonical_json_text(value) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
    except OSError as exc:
        raise build_error(ErrorCode.IO_ERROR, f"cannot append {target}", path=str(target), detail=str(exc)) from exc


def write_jsonl_atomic(path: str | os.PathLike[str], values: list[Any]) -> None:
    data = b"".join(canonical_json_bytes(value) + b"\n" for value in values)
    write_bytes_atomic(path, data)


def remove_tree_if_exists(path: Path) -> None:
    if not path.exists():
        return
    if path.is_dir():
        for child in sorted(path.rglob("*"), key=lambda item: len(item.parts), reverse=True):
            if child.is_file() or child.is_symlink():
                child.unlink(missing_ok=True)
            elif child.is_dir():
                child.rmdir()
        path.rmdir()
    else:
        path.unlink(missing_ok=True)

