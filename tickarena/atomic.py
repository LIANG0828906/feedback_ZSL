"""Cross-platform atomic replacement with bounded Windows sharing retries."""

from __future__ import annotations

import os
import time
from pathlib import Path


def replace_with_retry(
    source: str | Path,
    destination: str | Path,
    *,
    attempts: int = 40,
    delay_seconds: float = 0.025,
) -> None:
    """Atomically replace destination.

    Windows Defender/indexers can briefly hold a sharing lock on a small file
    immediately after it is written.  Retrying the same MoveFileEx-backed
    operation preserves atomicity; a delete-then-rename fallback would not.
    """

    source_path = Path(source)
    destination_path = Path(destination)
    for attempt in range(attempts):
        try:
            os.replace(source_path, destination_path)
            return
        except PermissionError:
            if attempt + 1 >= attempts:
                raise
            time.sleep(delay_seconds)

