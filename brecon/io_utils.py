from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from typing import Any

from .canonical import compact_json


def write_text_atomic(path: str | Path, text: str) -> None:
    """Write UTF-8 text through a same-directory temp file and atomic rename."""

    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    file_descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
    )
    try:
        with os.fdopen(file_descriptor, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, destination)
        if hasattr(os, "O_DIRECTORY"):
            try:
                directory_fd = os.open(destination.parent, os.O_DIRECTORY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def write_json_atomic(path: str | Path, value: Any) -> None:
    write_text_atomic(path, compact_json(value) + "\n")


def write_json_lines_atomic(path: str | Path, records: list[dict[str, Any]]) -> None:
    write_text_atomic(path, "".join(compact_json(record) + "\n" for record in records))


def load_json_file(path: str | Path) -> Any:
    with Path(path).open("r", encoding="utf-8") as handle:
        return json.load(handle)

