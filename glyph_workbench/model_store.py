from __future__ import annotations

import json
import os
import shutil
import tempfile
from pathlib import Path

from .errors import WorkbenchError
from .models import GlyphDocument, load_document


def save_document_atomic(document: GlyphDocument, path: str | Path) -> Path:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    payload = (
        json.dumps(
            document.to_dict(include_truth=True),
            ensure_ascii=True,
            indent=2,
            sort_keys=False,
        )
        + "\n"
    ).encode("utf-8")
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        directory_fd = os.open(destination.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError as exc:
        raise WorkbenchError(
            "E_IO",
            f"cannot atomically save model: {exc}",
            path=str(destination),
        ) from exc
    finally:
        if temporary.exists():
            temporary.unlink(missing_ok=True)
    return destination


def ensure_model_copy(source: str | Path, destination: str | Path) -> Path:
    source_path = Path(source)
    destination_path = Path(destination)
    if not destination_path.exists():
        destination_path.parent.mkdir(parents=True, exist_ok=True)
        temporary = destination_path.with_name(
            f".{destination_path.name}.{os.getpid()}.tmp"
        )
        shutil.copyfile(source_path, temporary)
        os.replace(temporary, destination_path)
    return destination_path


def load_model_or_copy(
    model_path: str | Path,
    bootstrap_path: str | Path,
) -> GlyphDocument:
    destination = Path(model_path)
    if not destination.exists():
        ensure_model_copy(bootstrap_path, destination)
    return load_document(destination)

