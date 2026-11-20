from __future__ import annotations

from pathlib import Path

import pytest

from volrender.samples import write_volume


@pytest.fixture()
def volume_path(tmp_path: Path) -> Path:
    path = tmp_path / "volume.svol"
    write_volume(path, dims=(16, 16, 12), spacing=(1.0, 1.0, 1.5))
    return path

